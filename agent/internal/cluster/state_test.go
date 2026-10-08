package cluster

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

func testNodes() []Node {
	return []Node{{ID: "a", RaftAddress: "127.0.0.1:9001", HTTPAddress: "a:8000", PostgresAddress: "a:5432", ContainerID: "ca"}, {ID: "b", RaftAddress: "127.0.0.1:9002", HTTPAddress: "b:8000", PostgresAddress: "b:5432", ContainerID: "cb"}, {ID: "c", RaftAddress: "127.0.0.1:9003", HTTPAddress: "c:8000", PostgresAddress: "c:5432", ContainerID: "cc"}}
}
func runCommand(f *fsm, c Command) error {
	b, _ := json.Marshal(c)
	r := f.Apply(&raft.Log{Data: b})
	if r == nil {
		return nil
	}
	return r.(error)
}
func bootCommand() Command {
	return Command{Version: 1, Kind: "bootstrap", ClusterID: "test", Primary: "a", At: "start"}
}
func beginCommand() Command {
	return Command{Version: 1, Kind: "begin", ClusterID: "test", ExpectedGeneration: 1, TransitionID: "t1", Candidate: "b", SystemID: "db", Timeline: 1, ReplayLSN: 100, OldContainerID: "ca", CandidateContainerID: "cb", At: "begin"}
}
func step(kind, phase string, g uint64) Command {
	return Command{Version: 1, Kind: kind, ClusterID: "test", TransitionID: "t1", ExpectedGeneration: g, ExpectedPhase: phase, At: kind}
}
func TestTransitionSafety(t *testing.T) {
	f := newFSM("test", testNodes())
	apply := func(c Command) {
		t.Helper()
		if e := runCommand(f, c); e != nil {
			t.Fatal(e)
		}
	}
	reject := func(c Command) {
		t.Helper()
		if e := runCommand(f, c); e == nil {
			t.Fatalf("accepted unsafe command %+v", c)
		}
	}
	apply(bootCommand())
	apply(bootCommand())
	apply(beginCommand())
	apply(beginCommand())
	competing := beginCommand()
	competing.TransitionID = "t2"
	competing.Candidate = "c"
	competing.CandidateContainerID = "cc"
	reject(competing)
	reject(step("authorize", "validating", 1))
	apply(step("fencing", "validating", 1))
	reject(step("authorize", "fencing", 1))
	auth := step("authorize", "fencing", 1)
	auth.FenceProof = "t1:ca"
	auth.SystemID = "db"
	auth.Timeline = 1
	auth.ReplayLSN = 99
	reject(auth)
	auth.ReplayLSN = 101
	apply(auth)
	apply(auth)
	if f.state.Generation != 2 || f.state.Primary != "b" {
		t.Fatal(f.state)
	}
	changed := auth
	changed.ReplayLSN = 102
	reject(changed)
	stale := step("promoting", "authorized", 1)
	reject(stale)
	apply(step("promoting", "authorized", 2))
	recon := step("reconfiguring", "promoting", 2)
	recon.Timeline = 2
	apply(recon)
	apply(step("complete", "reconfiguring", 2))
	apply(auth) // exact retries remain safe after later steps
	if f.state.Generation != 2 || f.state.Transition.Timeline != 2 || len(f.state.History) != 7 {
		t.Fatal(f.state)
	}
	next := beginCommand()
	next.TransitionID = "t2"
	next.ExpectedGeneration = 2
	next.ExpectedPhase = "complete"
	next.Candidate = "c"
	next.OldContainerID = "ca"
	next.CandidateContainerID = "cc"
	reject(next)
	next.OldContainerID = "cb"
	next.Timeline = 2
	apply(next)
	if f.state.Transition.OldPrimary != "b" {
		t.Fatal("forgot possible writer")
	}
}
func TestUnknownAndInvalidState(t *testing.T) {
	f := newFSM("test", testNodes())
	c := bootCommand()
	c.Version = 2
	if runCommand(f, c) == nil {
		t.Fatal("accepted future schema")
	}
	if err := f.Restore(io.NopCloser(strings.NewReader(`{"Version":99}`))); err == nil {
		t.Fatal("accepted future snapshot")
	}
	if got := f.Apply(&raft.Log{Data: []byte(`{"Version":1,"Kind":"bootstrap","ClusterID":"test","Primary":"a","mystery":true}`)}); got == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestUnknownPromotionOutcomeRetainsPossibleWriter(t *testing.T) {
	f := newFSM("test", testNodes())
	auth := step("authorize", "fencing", 1)
	auth.FenceProof = "t1:ca"
	auth.SystemID = "db"
	auth.Timeline = 1
	auth.ReplayLSN = 100
	for _, c := range []Command{bootCommand(), beginCommand(), step("fencing", "validating", 1), auth} {
		if err := runCommand(f, c); err != nil {
			t.Fatal(err)
		}
	}
	next := beginCommand()
	next.TransitionID = "t2"
	next.ExpectedGeneration = 2
	next.ExpectedPhase = "authorized"
	next.Candidate = "c"
	next.CandidateContainerID = "cc"
	next.OldContainerID = "cb"
	if err := runCommand(f, next); err != nil {
		t.Fatal(err)
	}
	if f.state.Primary != "b" || f.state.Transition.OldPrimary != "b" || f.state.Transition.OldContainerID != "cb" {
		t.Fatal("forgot uncertain writer")
	}
	stale := step("promoting", "authorized", 2)
	if err := runCommand(f, stale); err == nil {
		t.Fatal("resumed superseded promotion")
	}
	wrongFence := step("fencing", "validating", 2)
	wrongFence.TransitionID = "t2"
	if err := runCommand(f, wrongFence); err != nil {
		t.Fatal(err)
	}
	auth.TransitionID = "t2"
	auth.ExpectedGeneration = 2
	auth.FenceProof = "t2:ca"
	if err := runCommand(f, auth); err == nil {
		t.Fatal("authorized using original writer's fence")
	}
	auth.FenceProof = "t2:cb"
	if err := runCommand(f, auth); err != nil {
		t.Fatal(err)
	}
	if f.state.Primary != "c" || f.state.Generation != 3 {
		t.Fatal(f.state)
	}
}
func TestReinitializationExcludesCandidateUntilVerified(t *testing.T) {
	f := newFSM("test", testNodes())
	if err := runCommand(f, bootCommand()); err != nil {
		t.Fatal(err)
	}
	c := Command{Version: 1, ClusterID: "test", Kind: "reinitialize", TransitionID: "recover", NodeID: "b", ExpectedGeneration: 1}
	if err := runCommand(f, c); err != nil {
		t.Fatal(err)
	}
	if err := runCommand(f, beginCommand()); err == nil {
		t.Fatal("selected recovering candidate")
	}
	c.Kind = "reinitialized"
	if err := runCommand(f, c); err != nil {
		t.Fatal(err)
	}
	if err := runCommand(f, beginCommand()); err != nil {
		t.Fatal(err)
	}
}

// Each boundary is a distinct crash window. In particular, a completed external
// fence leaves the same durable fencing phase until authorization is committed;
// restore must never infer successful fencing or promotion from that phase.
func TestTransitionSnapshotCrashBoundaries(t *testing.T) {
	authorize := step("authorize", "fencing", 1)
	authorize.SystemID, authorize.Timeline, authorize.ReplayLSN = "db", 1, 101
	authorize.FenceProof = "t1:ca"
	reconfigure := step("reconfiguring", "promoting", 2)
	reconfigure.Timeline = 2
	commands := []Command{bootCommand(), beginCommand(), step("fencing", "validating", 1), authorize, step("promoting", "authorized", 2), reconfigure, step("complete", "reconfiguring", 2)}
	complete := newFSM("test", testNodes())
	for _, command := range commands {
		if err := runCommand(complete, command); err != nil {
			t.Fatal(err)
		}
	}
	for i, phase := range []string{"bootstrapped", "validating", "fencing", "authorized", "promoting", "reconfiguring", "complete"} {
		t.Run(phase, func(t *testing.T) {
			original := newFSM("test", testNodes())
			for _, command := range commands[:i+1] {
				if err := runCommand(original, command); err != nil {
					t.Fatal(err)
				}
			}
			before := original.stateCopy()
			generation, primary := uint64(1), "a"
			if i >= 3 {
				generation, primary = 2, "b"
			}
			if before.Generation != generation || before.Primary != primary {
				t.Fatalf("wrong authority at crash: %+v", before)
			}
			snap, err := original.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			defer snap.Release()
			// Mutating the live FSM after snapshot capture must not alter the snapshot.
			for _, command := range commands[i+1:] {
				if err := runCommand(original, command); err != nil {
					t.Fatal(err)
				}
			}
			disk, err := raft.NewFileSnapshotStore(t.TempDir(), 1, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			sink, err := disk.Create(1, uint64(i+1), 1, raft.Configuration{}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := snap.Persist(sink); err != nil {
				t.Fatal(err)
			}
			_, reader, err := disk.Open(sink.ID())
			if err != nil {
				t.Fatal(err)
			}
			restored := newFSM("test", testNodes())
			if err := restored.Restore(reader); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restored.stateCopy(), before) {
				t.Fatal("snapshot changed authority, identity, evidence, history, or receipts")
			}
			// Retried commits after a lost response remain exact no-ops after restore.
			for _, command := range commands[:i+1] {
				if err := runCommand(restored, command); err != nil {
					t.Fatalf("exact retry failed: %v", err)
				}
			}
			conflict := commands[i]
			conflict.At = "conflicting retry"
			if err := runCommand(restored, conflict); err == nil {
				t.Fatal("accepted conflicting retry after restore")
			}
			next := beginCommand()
			next.TransitionID, next.ExpectedPhase, next.ExpectedGeneration = "next", "complete", 2
			next.Candidate, next.CandidateContainerID, next.OldContainerID = "c", "cc", "cb"
			if i+1 < len(commands) {
				next = commands[i+1]
			}
			for _, corrupt := range []func(*Command){
				func(c *Command) { c.ExpectedGeneration = 0 },
				func(c *Command) { c.ExpectedPhase = "wrong-phase" },
			} {
				stale := next
				corrupt(&stale)
				if err := runCommand(restored, stale); err == nil {
					t.Fatal("accepted stale continuation after restore")
				}
			}
			if phase == "fencing" {
				noProof := authorize
				noProof.FenceProof = ""
				if err := runCommand(restored, noProof); err == nil {
					t.Fatal("restored fencing phase substituted for verified isolation")
				}
			}
			if !reflect.DeepEqual(restored.stateCopy(), before) {
				t.Fatal("retry or rejected command mutated restored state")
			}
			for _, command := range commands[i+1:] {
				if err := runCommand(restored, command); err != nil {
					t.Fatalf("resume failed: %v", err)
				}
			}
			if !reflect.DeepEqual(restored.stateCopy(), complete.stateCopy()) {
				t.Fatal("resumed transition differs from uninterrupted transition")
			}
		})
	}
}
