package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

type fakeDiscovery struct {
	targets []Target
	err     error
}

func (f *fakeDiscovery) Discover(context.Context) ([]Target, error) { return f.targets, f.err }

type fakeReader struct {
	mu               sync.Mutex
	values           map[string]Status
	failures         map[string]error
	entered, release chan struct{}
}

func (f *fakeReader) Read(ctx context.Context, t Target) (Status, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return Status{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[t.ID], f.failures[t.ID]
}

type fakeSession struct {
	target Target
	calls  int
}

func (f *fakeSession) Run(_ context.Context, t Target) error { f.target = t; f.calls++; return nil }
func fixture() (*App, *fakeDiscovery, *fakeReader, *fakeSession) {
	d := &fakeDiscovery{targets: []Target{{ID: "b", ClusterID: "lab", InstanceID: "opaque-b", Available: true}, {ID: "a", ClusterID: "lab", InstanceID: "opaque-a", Available: true}}}
	r := &fakeReader{values: map[string]Status{}, failures: map[string]error{}}
	for _, target := range d.targets {
		r.values[target.ID] = Status{Members: map[string]string{"a": "opaque-a", "b": "opaque-b"}, ID: target.ID, ClusterID: target.ClusterID, InstanceID: target.InstanceID, Healthy: true, Recovery: true, Generation: 7, Primary: "a", Leader: "b"}
	}
	s := &fakeSession{}
	return New(d, r, s), d, r, s
}
func TestRefreshRetainsSelectionAndStaleSnapshot(t *testing.T) {
	a, _, r, _ := fixture()
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := a.Snapshot(); len(v.Nodes) != 2 || v.Selected != "a" {
		t.Fatalf("initial view: %+v", v)
	}
	a.Move(1)
	r.failures["b"] = errors.New("offline")
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	v := a.Snapshot()
	if v.Selected != "b" || v.Nodes[1].Status == nil || v.Nodes[1].Error == "" {
		t.Fatalf("stale selection: %+v", v)
	}
	delete(r.failures, "b")
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v = a.Snapshot(); v.Selected != "b" || v.Nodes[1].Error != "" {
		t.Fatalf("recovery: %+v", v)
	}
}
func TestPinnedIdentityBlocksReplacementButAgentFailureAllowsSession(t *testing.T) {
	a, d, r, s := fixture()
	if err := a.OpenSession(t.Context()); err == nil {
		t.Fatal("unvalidated session allowed")
	}
	_ = a.Refresh(t.Context())
	a.Move(1)
	r.failures["b"] = errors.New("agent down")
	_ = a.Refresh(t.Context())
	if err := a.OpenSession(t.Context()); err != nil || s.target.ID != "b" {
		t.Fatalf("known node session: %v %+v", err, s)
	}
	d.targets[0].InstanceID = "replacement"
	_ = a.Refresh(t.Context())
	if err := a.OpenSession(t.Context()); err == nil || s.calls != 1 {
		t.Fatal("replacement session allowed")
	}
}
func TestLagDistinguishesZeroUnavailableAndStale(t *testing.T) {
	now := time.Now()
	zero := uint64(0)
	age := 30.0
	n := Node{Status: &Status{Healthy: true, Recovery: true, LagBytes: &zero, SampleAge: &age}, Received: now}
	if m := n.Lag(now); m.State != "current" || m.Bytes == nil || *m.Bytes != 0 {
		t.Fatalf("zero: %+v", m)
	}
	if m := n.Lag(now.Add(time.Millisecond)); m.State != "stale" {
		t.Fatalf("age boundary: %+v", m)
	}
	n.Status.SampleAge = nil
	if m := n.Lag(now); m.State != "unknown" {
		t.Fatalf("missing age: %+v", m)
	}
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1)} {
		n.Status.SampleAge = &invalid
		if m := n.Lag(now); m.State != "unknown" || m.Age != nil {
			t.Fatalf("invalid sample age accepted: %+v", m)
		}
	}
	n.Status.SampleAge = &age
	n.Error = "offline"
	if m := n.Lag(now); m.State != "stale" {
		t.Fatalf("snapshot stale: %+v", m)
	}
	n.Status.Recovery = false
	if m := n.Lag(now); m.State != "n/a" {
		t.Fatalf("primary: %+v", m)
	}
}
func TestPauseDiscardsLateResults(t *testing.T) {
	a, _, r, _ := fixture()
	_ = a.Refresh(t.Context())
	r.entered = make(chan struct{}, 2)
	r.release = make(chan struct{})
	done := make(chan struct{})
	go func() { _ = a.Refresh(t.Context()); close(done) }()
	<-r.entered
	a.Pause()
	close(r.release)
	<-done
	for _, n := range a.Snapshot().Nodes {
		if n.Error == "" {
			t.Fatal("pre-session response marked fresh")
		}
	}
	r.entered = nil
	r.release = nil
	a.Resume()
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, n := range a.Snapshot().Nodes {
		if n.Error != "" {
			t.Fatal("resume did not refresh")
		}
	}
}
func TestConsensusRejectsConflictingViews(t *testing.T) {
	a, _, r, _ := fixture()
	_ = a.Refresh(t.Context())
	if _, count, agree := a.Snapshot().Consensus(); count != 2 || !agree {
		t.Fatal("agreeing views missing")
	}
	s := r.values["b"]
	s.Generation++
	r.values["b"] = s
	_ = a.Refresh(t.Context())
	if _, _, agree := a.Snapshot().Consensus(); agree {
		t.Fatal("conflicting views treated as authority")
	}
}

func TestStatusIdentityMismatchBlocksSessionUntilValidatedRecovery(t *testing.T) {
	a, _, r, s := fixture()
	_ = a.Refresh(t.Context())
	bad := r.values["a"]
	bad.InstanceID = "other"
	r.values["a"] = bad
	_ = a.Refresh(t.Context())
	if err := a.OpenSession(t.Context()); err == nil || s.calls != 0 {
		t.Fatal("mismatched status permitted session")
	}
	r.failures["a"] = errors.New("offline")
	_ = a.Refresh(t.Context())
	if err := a.OpenSession(t.Context()); err == nil {
		t.Fatal("network failure cleared identity conflict")
	}
	delete(r.failures, "a")
	bad.InstanceID = "opaque-a"
	r.values["a"] = bad
	_ = a.Refresh(t.Context())
	if err := a.OpenSession(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingRefreshDoesNotReviveRetainedSnapshots(t *testing.T) {
	for _, sessionReturn := range []bool{false, true} {
		t.Run(fmt.Sprint(sessionReturn), func(t *testing.T) {
			a, _, r, _ := fixture()
			_ = a.Refresh(t.Context())
			if sessionReturn {
				a.Pause()
				a.Resume()
			} else {
				for id := range r.values {
					r.failures[id] = errors.New("offline")
				}
				_ = a.Refresh(t.Context())
				clear(r.failures)
			}
			r.entered = make(chan struct{}, 2)
			r.release = make(chan struct{})
			done := make(chan struct{})
			go func() { _ = a.Refresh(t.Context()); close(done) }()
			<-r.entered
			<-r.entered
			view := a.Snapshot()
			_, count, _ := view.Consensus()
			failed := count != 0 || view.Nodes[0].Error == "" || view.Nodes[1].Error == ""
			close(r.release)
			<-done
			if failed {
				t.Fatal("pending HTTP read made retained snapshot current")
			}
			if _, count, agree := a.Snapshot().Consensus(); count != 2 || !agree {
				t.Fatal("valid response did not recover")
			}
		})
	}
}

func TestMembershipConflictsBlockSelectedSession(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			a, _, r, _ := fixture()
			_ = a.Refresh(t.Context())
			s := r.values["a"]
			s.Members = map[string]string{"a": "opaque-a", "b": "opaque-b"}
			if extra {
				s.Members["other"] = "unexpected"
			} else {
				s.Members["b"] = "wrong-container"
			}
			r.values["a"] = s
			_ = a.Refresh(t.Context())
			if a.Snapshot().Nodes[0].Error == "" {
				t.Fatal("conflicting peer binding accepted")
			}
			if err := a.OpenSession(t.Context()); err == nil {
				t.Fatal("membership conflict allowed psql")
			}
		})
	}
}
