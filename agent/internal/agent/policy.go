package agent

import (
	"fmt"
	"time"
)

// Evidence is the controller's locally timed view of a fresh database observation.
// ReceivedAt is set to the request start, never a timestamp supplied by a peer.
type Evidence struct {
	NodeID, ContainerID, Incarnation, SystemID, RecoveryState string
	Generation, Timeline, FlushLSN, ReplayLSN                 uint64
	Healthy, Recovery, ReplayPaused                           bool
	ReceivedAt                                                time.Time
}

func Eligible(candidate, primary Evidence, generation, maxLag uint64, maxAge time.Duration, now time.Time) (uint64, error) {
	for _, s := range []Evidence{candidate, primary} {
		age := now.Sub(s.ReceivedAt)
		if s.ReceivedAt.IsZero() || age < 0 || age > maxAge {
			return 0, fmt.Errorf("missing or stale observation")
		}
		if s.NodeID == "" || s.ContainerID == "" || s.Incarnation == "" || s.SystemID == "" || s.Generation != generation {
			return 0, fmt.Errorf("observation identity or generation mismatch")
		}
	}
	if !candidate.Healthy || !candidate.Recovery || candidate.ReplayPaused || candidate.RecoveryState != "" {
		return 0, fmt.Errorf("candidate is unhealthy, excluded, paused, or not in recovery")
	}
	if !primary.Healthy || primary.Recovery || primary.FlushLSN == 0 || candidate.ReplayLSN == 0 {
		return 0, fmt.Errorf("missing valid primary or replay position")
	}
	if candidate.NodeID == primary.NodeID || candidate.SystemID != primary.SystemID || candidate.Timeline == 0 || candidate.Timeline != primary.Timeline {
		return 0, fmt.Errorf("incompatible database identity or timeline")
	}
	var lag uint64
	if primary.FlushLSN > candidate.ReplayLSN {
		lag = primary.FlushLSN - candidate.ReplayLSN
	}
	if lag > maxLag {
		return lag, fmt.Errorf("observed lag %d exceeds limit %d", lag, maxLag)
	}
	return lag, nil
}
