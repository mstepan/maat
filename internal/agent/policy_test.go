package agent

import (
	"testing"
	"time"
)

func TestEligibilityRejectsStaleUnsafeAndOverLimitCandidates(t *testing.T) {
	now := time.Now()
	p := Evidence{NodeID: "a", ContainerID: "aa", Incarnation: "pa", SystemID: "123", Generation: 1, Timeline: 1, FlushLSN: 33554432, Healthy: true, ReceivedAt: now}
	c := Evidence{NodeID: "b", ContainerID: "bb", Incarnation: "pb", SystemID: "123", Generation: 1, Timeline: 1, ReplayLSN: 16777216, Healthy: true, Recovery: true, ReceivedAt: now}
	if lag, e := Eligible(c, p, 1, 16777216, 30*time.Second, now); e != nil || lag != 16777216 {
		t.Fatalf("boundary rejected: %d %v", lag, e)
	}
	for _, name := range []string{"lag", "stale_candidate", "stale_primary", "missing_primary", "identity", "timeline", "agent_incarnation", "not_recovery", "unhealthy", "paused", "generation", "recovering", "future"} {
		t.Run(name, func(t *testing.T) {
			a, b := c, p
			switch name {
			case "lag":
				a.ReplayLSN--
			case "stale_candidate":
				a.ReceivedAt = now.Add(-31 * time.Second)
			case "stale_primary":
				b.ReceivedAt = now.Add(-31 * time.Second)
			case "missing_primary":
				b.FlushLSN = 0
			case "identity":
				a.SystemID = "456"
			case "timeline":
				a.Timeline = 2
			case "agent_incarnation":
				a.Incarnation = ""
			case "not_recovery":
				a.Recovery = false
			case "unhealthy":
				a.Healthy = false
			case "paused":
				a.ReplayPaused = true
			case "generation":
				a.Generation = 0
			case "recovering":
				a.RecoveryState = "rewinding"
			case "future":
				a.ReceivedAt = now.Add(time.Second)
			}
			if _, e := Eligible(a, b, 1, 16777216, 30*time.Second, now); e == nil {
				t.Fatal("unsafe candidate accepted")
			}
		})
	}
	c.ReplayLSN = p.FlushLSN + 1024
	if lag, e := Eligible(c, p, 1, 0, 30*time.Second, now); e != nil || lag != 0 {
		t.Fatalf("replica beyond sampled primary incorrectly rejected: %d %v", lag, e)
	}
}
