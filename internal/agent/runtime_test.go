package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maat/internal/cluster"
	"maat/internal/fencing"
	"maat/internal/postgres"
)

func TestRestartedAgentCannotReuseRetainedPrimaryEvidence(t *testing.T) {
	phase := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase == 1 {
			http.Error(w, "offline", 503)
			return
		}
		o := Observation{NodeID: "a", ClusterID: "test", ContainerID: "container", Incarnation: "old", Generation: 1, Database: postgres.Observation{Healthy: true, SystemID: "123", Timeline: 1, FlushLSN: 100}}
		if phase == 2 {
			o.Incarnation = "new"
			o.Database.Healthy = false
			o.Error = "unavailable"
		}
		writeJSON(w, o)
	}))
	defer server.Close()
	a := &Runtime{cfg: Config{ID: "b", ClusterID: "test", ObservationIntervalSeconds: 1, Nodes: []Node{{ID: "a", HTTPAddress: strings.TrimPrefix(server.URL, "http://")}}}, targets: map[string]fencing.Target{"a": {ContainerID: "container"}}, http: server.Client(), samples: map[string]sample{}, primaryEvidence: map[string]Evidence{}, failures: map[string]int{}}
	a.poll(context.Background())
	if a.primaryEvidence["a"].Incarnation != "old" {
		t.Fatal("did not collect primary evidence")
	}
	phase = 1
	a.poll(context.Background())
	if _, ok := a.samples["a"]; ok {
		t.Fatal("unavailable peer kept live sample")
	}
	phase = 2
	a.poll(context.Background())
	if _, ok := a.primaryEvidence["a"]; ok {
		t.Fatal("restart reused pre-crash primary evidence")
	}
}
func TestReinitializeRequiresCurrentAuthorityAndExplicitAcknowledgement(t *testing.T) {
	s := cluster.State{Generation: 4, Primary: "a", Nodes: []cluster.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	for _, req := range []ReinitializeRequest{{NodeID: "b", Generation: 4}, {NodeID: "a", Generation: 4, Acknowledge: true}, {NodeID: "b", Generation: 3, Acknowledge: true}, {NodeID: "outsider", Generation: 4, Acknowledge: true}} {
		if e := validateReinitialize(s, req); e == nil {
			t.Fatalf("unsafe recovery request accepted: %+v", req)
		}
	}
	req := ReinitializeRequest{NodeID: "b", Generation: 4, Acknowledge: true}
	if e := validateReinitialize(s, req); e != nil {
		t.Fatal(e)
	}
	s.Transition = &cluster.Transition{Phase: "promoting"}
	if e := validateReinitialize(s, req); e == nil {
		t.Fatal("recovery started during promotion")
	}
}
func TestEvidenceAgeUsesRequestStart(t *testing.T) {
	started := time.Now().Add(-time.Second)
	got := evidence(sample{Observation: Observation{NodeID: "a", Incarnation: "one"}, Started: started})
	if !got.ReceivedAt.Equal(started) {
		t.Fatal("sample was refreshed during conversion")
	}
}

func TestFencedOldPrimaryCannotRestartDuringPromotion(t *testing.T) {
	s := cluster.State{Primary: "a", Transition: &cluster.Transition{OldPrimary: "a", Candidate: "b", Phase: "fencing"}}
	if !blocksRestart(s, "a") {
		t.Fatal("fenced old primary allowed to restart while replacement is pending")
	}
	s.Primary = "b"
	s.Transition.Phase = "authorized"
	if !blocksRestart(s, "a") {
		t.Fatal("old primary allowed to restart before promotion verification")
	}
	if blocksRestart(s, "b") {
		t.Fatal("authorized candidate incorrectly blocked")
	}
	s.Transition.Phase = "reconfiguring"
	if blocksRestart(s, "a") {
		t.Fatal("old replica cannot begin verified rejoin after promotion")
	}
}
func TestStoppedAuthorizedCandidateCanRegainLeadership(t *testing.T) {
	now := time.Now()
	s := cluster.State{Generation: 2, Transition: &cluster.Transition{Phase: "authorized", Candidate: "b", CandidateContainerID: "bb"}}
	o := sample{Observation: Observation{NodeID: "b", ContainerID: "bb", Generation: 2, Incarnation: "restarted", Error: "PostgreSQL observation unavailable"}, Started: now}
	if e := candidateAgentReady(s, o, now, time.Minute); e != nil {
		t.Fatalf("live candidate with stopped database rejected: %v", e)
	}
	s.Transition.Phase = "reconfiguring"
	if e := candidateAgentReady(s, o, now, time.Minute); e != nil {
		t.Fatal("stopped promoted candidate cannot resume reconfiguration", e)
	}
	o.Started = now.Add(-2 * time.Minute)
	if e := candidateAgentReady(s, o, now, time.Minute); e == nil {
		t.Fatal("stale candidate agent accepted")
	}
}

func TestFailedRecoveryRetryRequiresMatchingFreshFailure(t *testing.T) {
	now := time.Now()
	s := cluster.State{Generation: 2, Nodes: []cluster.Node{{ID: "a", ContainerID: "aa"}}}
	o := sample{Observation: Observation{NodeID: "a", ContainerID: "aa", Incarnation: "live", Generation: 2, RecoveryState: "reinitialization_required", RecoveryRequestID: "request"}, Started: now}
	if !failedRecovery(s, "a", "request", o, now, time.Minute) {
		t.Fatal("failed current recovery cannot be retried")
	}
	o.RecoveryRequestID = "earlier"
	if failedRecovery(s, "a", "request", o, now, time.Minute) {
		t.Fatal("unrelated recovery failure permits retry")
	}
	o.RecoveryRequestID = "request"
	o.Started = now.Add(-2 * time.Minute)
	if failedRecovery(s, "a", "request", o, now, time.Minute) {
		t.Fatal("stale failure permits retry")
	}
}
