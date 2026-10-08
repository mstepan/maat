package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftbolt "github.com/hashicorp/raft-boltdb/v2"
)

type Config struct {
	ID, ClusterID, Bind, Dir string
	Nodes                    []Node
	Bootstrap                bool
}
type Store struct {
	raft      *raft.Raft
	fsm       *fsm
	transport *raft.NetworkTransport
	bolt      *raftbolt.BoltStore
	once      sync.Once
	closeErr  error
}

func Start(c Config) (*Store, error) {
	c.Nodes = append([]Node(nil), c.Nodes...)
	sort.Slice(c.Nodes, func(i, j int) bool { return c.Nodes[i].ID < c.Nodes[j].ID })
	if c.ID == "" || c.ClusterID == "" || c.Dir == "" || c.Bind == "" || len(c.Nodes) != 3 {
		return nil, errors.New("raft requires identity, storage, bind address and exactly three nodes")
	}
	ids := map[string]bool{}
	addrs := map[string]bool{}
	containers := map[string]bool{}
	local := false
	for _, n := range c.Nodes {
		if n.ID == "" || n.ContainerID == "" || ids[n.ID] || addrs[n.RaftAddress] || containers[n.ContainerID] {
			return nil, errors.New("missing or duplicated Raft node identity")
		}
		if _, _, err := net.SplitHostPort(n.RaftAddress); err != nil {
			return nil, fmt.Errorf("invalid Raft address: %w", err)
		}
		ids[n.ID] = true
		addrs[n.RaftAddress] = true
		containers[n.ContainerID] = true
		local = local || n.ID == c.ID
	}
	if !local {
		return nil, errors.New("local node absent from membership")
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return nil, err
	}
	bolt, err := raftbolt.NewBoltStore(filepath.Join(c.Dir, "raft.db"))
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = bolt.Close()
		}
	}()
	snapshots, err := raft.NewFileSnapshotStore(c.Dir, 2, io.Discard)
	if err != nil {
		return nil, err
	}
	existing, err := raft.HasExistingState(bolt, bolt, snapshots)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(struct {
		Version       int
		ID, ClusterID string
		Nodes         []Node
	}{Version, c.ID, c.ClusterID, c.Nodes})
	stored, err := bolt.Get([]byte("maat-identity"))
	if err != nil && !errors.Is(err, raftbolt.ErrKeyNotFound) {
		return nil, err
	}
	if len(stored) > 0 && !bytes.Equal(stored, identity) {
		return nil, errors.New("persisted Raft identity or topology differs from configuration")
	}
	if len(stored) == 0 {
		if existing {
			return nil, errors.New("existing Raft state has no cluster identity")
		}
		if err := bolt.Set([]byte("maat-identity"), identity); err != nil {
			return nil, err
		}
	}
	// Reject unknown persisted formats before Raft can replay or fall back from
	// an unreadable latest snapshot to older authority.
	first, err := bolt.FirstIndex()
	if err != nil {
		return nil, err
	}
	last, err := bolt.LastIndex()
	if err != nil {
		return nil, err
	}
	for index := first; index > 0 && index <= last; index++ {
		var entry raft.Log
		if err := bolt.GetLog(index, &entry); err != nil {
			return nil, err
		}
		if entry.Type == raft.LogCommand {
			var command Command
			if err := strictJSON(entry.Data, &command); err != nil {
				return nil, err
			}
			if command.Version != Version || command.ClusterID != c.ClusterID {
				return nil, errors.New("persisted Raft command has unknown schema or cluster identity")
			}
		}
	}
	listed, err := snapshots.List()
	if err != nil {
		return nil, err
	}
	if len(listed) > 0 {
		_, reader, err := snapshots.Open(listed[0].ID)
		if err != nil {
			return nil, err
		}
		if err := newFSM(c.ClusterID, c.Nodes).Restore(reader); err != nil {
			return nil, err
		}
	}
	var advertised *net.TCPAddr
	for _, n := range c.Nodes {
		if n.ID == c.ID {
			advertised, err = net.ResolveTCPAddr("tcp", n.RaftAddress)
			if err != nil {
				return nil, err
			}
		}
	}
	transport, err := raft.NewTCPTransport(c.Bind, advertised, 3, 2*time.Second, io.Discard)
	if err != nil {
		return nil, err
	}
	transportCleanup := true
	defer func() {
		if transportCleanup {
			_ = transport.Close()
		}
	}()
	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(c.ID)
	rc.LogOutput = io.Discard
	f := newFSM(c.ClusterID, c.Nodes)
	r, err := raft.NewRaft(rc, f, bolt, bolt, snapshots, transport)
	if err != nil {
		return nil, err
	}
	if !existing && c.Bootstrap {
		membership := raft.Configuration{}
		for _, n := range c.Nodes {
			membership.Servers = append(membership.Servers, raft.Server{ID: raft.ServerID(n.ID), Address: raft.ServerAddress(n.RaftAddress), Suffrage: raft.Voter})
		}
		if err := r.BootstrapCluster(membership).Error(); err != nil {
			_ = r.Shutdown().Error()
			return nil, err
		}
	}
	cleanup = false
	transportCleanup = false
	return &Store{raft: r, fsm: f, transport: transport, bolt: bolt}, nil
}
func (s *Store) Close() error {
	s.once.Do(func() { s.closeErr = errors.Join(s.raft.Shutdown().Error(), s.transport.Close(), s.bolt.Close()) })
	return s.closeErr
}
func (s *Store) State() State     { return s.fsm.stateCopy() }
func (s *Store) IsLeader() bool   { return s.raft.State() == raft.Leader }
func (s *Store) LeaderID() string { _, id := s.raft.LeaderWithID(); return string(id) }
func (s *Store) Term() uint64     { v, _ := strconv.ParseUint(s.raft.Stats()["term"], 10, 64); return v }
func wait(ctx context.Context, f raft.Future) error {
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func timeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return max(time.Until(deadline), time.Nanosecond)
	}
	return 5 * time.Second
}
func (s *Store) Apply(ctx context.Context, c Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	future := s.raft.Apply(data, timeout(ctx))
	if err := wait(ctx, future); err != nil {
		return err
	}
	if response := future.Response(); response != nil {
		if err, ok := response.(error); ok {
			return err
		}
		return errors.New("unexpected Raft FSM response")
	}
	return nil
}

// LinearizableState is leader-only. Followers may serve diagnostic State, but
// decisions must obtain this quorum-confirmed view through the current leader.
func (s *Store) LinearizableState(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := wait(ctx, s.raft.VerifyLeader()); err != nil {
		return State{}, err
	}
	if err := wait(ctx, s.raft.Barrier(timeout(ctx))); err != nil {
		return State{}, err
	}
	if !s.IsLeader() {
		return State{}, raft.ErrNotLeader
	}
	return s.State(), nil
}
func (s *Store) Transfer(ctx context.Context, nodeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, n := range s.State().Nodes {
		if n.ID == nodeID {
			return wait(ctx, s.raft.LeadershipTransferToServer(raft.ServerID(n.ID), raft.ServerAddress(n.RaftAddress)))
		}
	}
	return errors.New("leadership transfer target is not a member")
}
