package cluster

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	raftbolt "github.com/hashicorp/raft-boltdb/v2"
)

func TestDurableRaftRestartTransferAndQuorum(t *testing.T) {
	nodes := testNodes()
	// Reserve together to avoid assigning the same ephemeral address twice.
	listeners := make([]net.Listener, 3)
	for i := range nodes {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = l
		nodes[i].RaftAddress = l.Addr().String()
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	dir := t.TempDir()
	stores := make([]*Store, 3)
	start := func(i int) {
		t.Helper()
		s, err := Start(Config{ID: nodes[i].ID, ClusterID: "test", Bind: nodes[i].RaftAddress, Dir: filepath.Join(dir, nodes[i].ID), Nodes: nodes, Bootstrap: i == 0})
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}
	stop := func(i int) {
		t.Helper()
		if stores[i] != nil {
			if err := stores[i].Close(); err != nil {
				t.Fatal(err)
			}
			stores[i] = nil
		}
	}
	t.Cleanup(func() {
		for i := range stores {
			stop(i)
		}
	})
	for i := range stores {
		start(i)
	}
	leader := func() int {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for i, s := range stores {
				if s != nil && s.IsLeader() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					_, err := s.LinearizableState(ctx)
					cancel()
					if err == nil {
						return i
					}
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("no quorum-backed leader")
		return -1
	}
	apply := func(c Command) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := stores[leader()].Apply(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	apply(bootCommand())
	apply(beginCommand())
	apply(step("fencing", "validating", 1))
	auth := step("authorize", "fencing", 1)
	auth.FenceProof = "t1:ca"
	auth.SystemID = "db"
	auth.Timeline = 1
	auth.ReplayLSN = 101
	apply(auth)
	li := leader()
	target := (li + 1) % 3
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := stores[li].Transfer(ctx, nodes[target].ID)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if got := leader(); got != target {
		t.Fatalf("transferred to %d, wanted %d", got, target)
	}
	// Wait for all real FSMs, then force file snapshots to exercise restore too.
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := true
		for _, s := range stores {
			all = all && s.State().Generation == 2
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("followers did not apply authorization")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := stores[target].raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	} // Other members restore from their durable logs.
	for i := range stores {
		stop(i)
	}
	for i := range stores {
		start(i)
	}
	li = leader()
	state := stores[li].State()
	if state.Generation != 2 || state.Primary != "b" || state.Transition.Phase != "authorized" || state.Transition.OldContainerID != "ca" {
		t.Fatalf("lost durable state: %+v", state)
	}
	// A returned snapshot is owned by the caller, never an alias into authority.
	state.Nodes[0].ID = "bad"
	state.Transition.Candidate = "bad"
	state.Receipts["/bootstrap"] = "bad"
	if stores[li].State().Nodes[0].ID != "a" || stores[li].State().Transition.Candidate != "b" {
		t.Fatal("State exposed mutable authority")
	}
	apply(auth)
	for i := range stores {
		if i != li {
			stop(i)
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	_, err = stores[li].LinearizableState(ctx)
	cancel()
	if err == nil {
		t.Fatal("linearizable read passed without majority")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = stores[li].Apply(ctx, step("promoting", "authorized", 2))
	cancel()
	if err == nil {
		t.Fatal("command passed without majority")
	}
	if s := stores[li].State(); s.Generation != 2 || s.Transition.Phase != "authorized" {
		t.Fatal("majority loss changed authority")
	}
	stop(li)
	// Future-schema log entries must fail startup, rather than silently leave
	// generation zero when Raft ignores an FSM command error during replay.
	backend, err := raftbolt.NewBoltStore(filepath.Join(dir, nodes[li].ID, "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	last, err := backend.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.StoreLog(&raft.Log{Index: last + 1, Term: 100, Type: raft.LogCommand, Data: []byte(`{"Version":99,"ClusterID":"test"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Start(Config{ID: nodes[li].ID, ClusterID: "test", Bind: nodes[li].RaftAddress, Dir: filepath.Join(dir, nodes[li].ID), Nodes: nodes, Bootstrap: true}); err == nil {
		_ = s.Close()
		t.Fatal("restarted with unknown persisted command schema")
	}
	changed := append([]Node(nil), nodes...)
	changed[0].ContainerID = "replacement"
	if s, err := Start(Config{ID: nodes[li].ID, ClusterID: "test", Bind: nodes[li].RaftAddress, Dir: filepath.Join(dir, nodes[li].ID), Nodes: changed, Bootstrap: true}); err == nil {
		_ = s.Close()
		t.Fatal("restarted with different container identity")
	}
}
