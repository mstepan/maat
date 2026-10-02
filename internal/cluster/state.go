// Package cluster persists control-plane authority. PostgreSQL observations and
// live fencing verification remain the trusted reconciler's responsibility.
package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"

	"github.com/hashicorp/raft"
)

const Version = 1

type Node struct{ ID, RaftAddress, HTTPAddress, PostgresAddress, ContainerID string }
type Transition struct {
	ID, OldPrimary, Candidate, Phase, SystemID                             string
	Timeline, ReplayLSN                                                    uint64
	OldContainerID, CandidateContainerID, FenceProof, StartedAt, UpdatedAt string
}
type Reinitialization struct {
	ID, NodeID string
	Generation uint64
	At         string
}
type Event struct {
	Transition                             *Transition
	Kind, TransitionID, Primary, Phase, At string
	Generation                             uint64
}
type State struct {
	Version           int
	ClusterID         string
	Generation        uint64
	Primary           string
	Nodes             []Node
	Transition        *Transition
	History           []Event
	Reinitializations map[string]Reinitialization
	Receipts          map[string]string
}
type Command struct {
	Version                                                      int
	Kind, ClusterID, TransitionID, ExpectedPhase                 string
	ExpectedGeneration                                           uint64
	Primary, Candidate, SystemID                                 string
	Timeline, ReplayLSN                                          uint64
	OldContainerID, CandidateContainerID, FenceProof, At, NodeID string
}

type fsm struct {
	mu        sync.RWMutex
	state     State
	clusterID string
	nodes     []Node
}

func newFSM(id string, nodes []Node) *fsm {
	return &fsm{clusterID: id, nodes: append([]Node(nil), nodes...), state: State{Version: Version, ClusterID: id, Nodes: append([]Node(nil), nodes...), Reinitializations: map[string]Reinitialization{}, Receipts: map[string]string{}}}
}
func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
func (f *fsm) Apply(log *raft.Log) any {
	var c Command
	if err := strictJSON(log.Data, &c); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Version != Version || c.ClusterID != f.clusterID {
		return errors.New("unsupported command version or cluster identity")
	}
	canonical, _ := json.Marshal(c)
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	key := c.TransitionID + "/" + c.Kind
	if previous, ok := f.state.Receipts[key]; ok {
		if previous == digest {
			return nil
		}
		return errors.New("conflicting retry")
	}
	if c.ExpectedGeneration != f.state.Generation {
		return errors.New("stale generation")
	}
	if err := f.advance(c); err != nil {
		return err
	}
	f.state.Receipts[key] = digest
	phase := ""
	if f.state.Transition != nil {
		phase = f.state.Transition.Phase
	}
	var transition *Transition
	if f.state.Transition != nil {
		copied := *f.state.Transition
		transition = &copied
	}
	f.state.History = append(f.state.History, Event{Transition: transition, Kind: c.Kind, TransitionID: c.TransitionID, Primary: f.state.Primary, Phase: phase, At: c.At, Generation: f.state.Generation})
	return nil
}
func (f *fsm) node(id string) (Node, bool) {
	for _, n := range f.state.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}
func (f *fsm) advance(c Command) error {
	s := &f.state
	switch c.Kind {
	case "bootstrap":
		if s.Generation != 0 || s.Primary != "" || c.ExpectedPhase != "" || c.TransitionID != "" {
			return errors.New("cluster already initialized or invalid bootstrap")
		}
		if _, ok := f.node(c.Primary); !ok {
			return errors.New("unknown initial primary")
		}
		s.Generation = 1
		s.Primary = c.Primary
		return nil
	case "reinitialized":
		r, ok := s.Reinitializations[c.NodeID]
		if !ok || r.ID != c.TransitionID || r.Generation != s.Generation || c.NodeID == s.Primary {
			return errors.New("stale recovery completion")
		}
		delete(s.Reinitializations, c.NodeID)
		return nil
	case "reinitialize":
		if s.Generation == 0 || c.TransitionID == "" || c.NodeID == s.Primary {
			return errors.New("invalid reinitialization target")
		}
		if _, ok := f.node(c.NodeID); !ok {
			return errors.New("unknown reinitialization target")
		}
		if s.Transition != nil && s.Transition.Phase != "complete" {
			return errors.New("transition in progress")
		}
		s.Reinitializations[c.NodeID] = Reinitialization{ID: c.TransitionID, NodeID: c.NodeID, Generation: s.Generation, At: c.At}
		return nil
	case "begin":
		if s.Generation == 0 || c.TransitionID == "" || c.Candidate == s.Primary || c.SystemID == "" || c.Timeline == 0 || c.ReplayLSN == 0 {
			return errors.New("invalid candidate evidence")
		}
		// A replacement of an uncertain authorized writer must target that current
		// writer. Pre-authorization attempts cannot be overwritten concurrently.
		if s.Transition != nil && (s.Transition.Phase == "validating" || s.Transition.Phase == "fencing") {
			return errors.New("transition in progress")
		}
		if s.Transition == nil {
			if c.ExpectedPhase != "" {
				return errors.New("unexpected phase")
			}
		} else if c.ExpectedPhase != s.Transition.Phase {
			return errors.New("stale phase")
		}
		old, ok := f.node(s.Primary)
		if !ok {
			return errors.New("unknown old primary")
		}
		candidate, ok := f.node(c.Candidate)
		if !ok {
			return errors.New("unknown candidate")
		}
		if old.ContainerID == "" || candidate.ContainerID == "" || c.OldContainerID != old.ContainerID || c.CandidateContainerID != candidate.ContainerID {
			return errors.New("container incarnation mismatch")
		}
		if r, ok := s.Reinitializations[c.Candidate]; ok && r.Generation == s.Generation {
			return errors.New("candidate has recovery request")
		}
		s.Transition = &Transition{ID: c.TransitionID, OldPrimary: s.Primary, Candidate: c.Candidate, Phase: "validating", SystemID: c.SystemID, Timeline: c.Timeline, ReplayLSN: c.ReplayLSN, OldContainerID: c.OldContainerID, CandidateContainerID: c.CandidateContainerID, StartedAt: c.At, UpdatedAt: c.At}
		return nil
	}
	t := s.Transition
	if t == nil || c.TransitionID != t.ID || c.ExpectedPhase != t.Phase {
		return errors.New("stale transition or phase")
	}
	switch c.Kind {
	case "fencing":
		if t.Phase != "validating" {
			return errors.New("fencing requires validation")
		}
		t.Phase = "fencing"
	case "authorize":
		if t.Phase != "fencing" || s.Primary != t.OldPrimary {
			return errors.New("authorization requires fencing current primary")
		}
		// The proof binds independently verified isolation to this exact transition
		// and immutable container. It is an assertion from trusted runtime code,
		// never accepted from a public administrative endpoint.
		if c.FenceProof != t.ID+":"+t.OldContainerID || c.SystemID != t.SystemID || c.Timeline != t.Timeline || c.ReplayLSN < t.ReplayLSN {
			return errors.New("missing fencing proof or invalid candidate watermark")
		}
		if s.Generation == ^uint64(0) {
			return errors.New("generation exhausted")
		}
		t.FenceProof = c.FenceProof
		t.ReplayLSN = c.ReplayLSN
		t.Phase = "authorized"
		s.Generation++
		s.Primary = t.Candidate
	case "promoting":
		if t.Phase != "authorized" || s.Primary != t.Candidate {
			return errors.New("promotion requires current authorization")
		}
		t.Phase = "promoting"
	case "reconfiguring":
		if t.Phase != "promoting" || s.Primary != t.Candidate || c.Timeline <= t.Timeline {
			return errors.New("verified promotion must advance PostgreSQL timeline")
		}
		t.Timeline = c.Timeline
		t.Phase = "reconfiguring"
	case "complete":
		if t.Phase != "reconfiguring" {
			return errors.New("completion requires replica reconfiguration")
		}
		t.Phase = "complete"
	default:
		return fmt.Errorf("unknown command %q", c.Kind)
	}
	t.UpdatedAt = c.At
	return nil
}
func (f *fsm) stateCopy() State {
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, _ := json.Marshal(f.state)
	var s State
	_ = json.Unmarshal(b, &s)
	return s
}
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	b, err := json.Marshal(f.stateCopy())
	return snapshot(b), err
}
func (f *fsm) Restore(r io.ReadCloser) error {
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return err
	}
	var s State
	if err := strictJSON(b, &s); err != nil {
		return err
	}
	if s.Version != Version || s.ClusterID != f.clusterID || !reflect.DeepEqual(s.Nodes, f.nodes) {
		return errors.New("snapshot schema, cluster identity or topology mismatch")
	}
	if s.Receipts == nil || s.Reinitializations == nil {
		return errors.New("invalid snapshot maps")
	}
	if (s.Generation == 0) != (s.Primary == "") {
		return errors.New("invalid snapshot authority")
	}
	if s.Primary != "" {
		found := false
		for _, n := range s.Nodes {
			found = found || n.ID == s.Primary
		}
		if !found {
			return errors.New("snapshot primary is not a member")
		}
	}
	if s.Transition != nil {
		switch s.Transition.Phase {
		case "validating", "fencing", "authorized", "promoting", "reconfiguring", "complete":
		default:
			return errors.New("unknown snapshot transition phase")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
	return nil
}

type snapshot []byte

func (s snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel()
		return err
	}
	if err := sink.Close(); err != nil {
		_ = sink.Cancel()
		return err
	}
	return nil
}
func (snapshot) Release() {}
