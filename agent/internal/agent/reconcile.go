package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"maat/internal/cluster"
	"maat/internal/fencing"
)

func (a *Runtime) command(ctx context.Context, s cluster.State, kind string, mutate func(*cluster.Command)) error {
	c := cluster.Command{Version: cluster.Version, ClusterID: a.cfg.ClusterID, Kind: kind, ExpectedGeneration: s.Generation, At: time.Now().UTC().Format(time.RFC3339Nano)}
	if s.Transition != nil {
		c.ExpectedPhase = s.Transition.Phase
		c.TransitionID = s.Transition.ID
	}
	if mutate != nil {
		mutate(&c)
	}
	if e := a.fault(ctx, "before-"+kind); e != nil {
		return e
	}
	if e := a.store.Apply(ctx, c); e != nil {
		return e
	}
	return a.fault(ctx, "after-"+kind)
}
func (a *Runtime) reconcile(ctx context.Context) error {
	s, e := a.authority(ctx)
	if e != nil {
		return e
	}
	local := a.localObservation(ctx)
	if local.Database.Healthy && !local.Database.Recovery && s.Primary != a.cfg.ID {
		if e = a.pg.Stop(ctx); e != nil {
			return e
		}
		return errors.New("unauthorized writable PostgreSQL stopped; recovery required")
	}
	if s.Generation == 0 {
		if !a.store.IsLeader() {
			return errors.New("waiting for initial cluster authorization")
		}
		for _, n := range a.cfg.Nodes {
			o, ok := a.sample(n.ID)
			if !ok || !o.Empty || o.Generation != 0 || o.RecoveryState != "" || time.Since(o.Started) > time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second {
				return errors.New("bootstrap requires fresh empty storage on all three nodes")
			}
		}
		return a.command(ctx, s, "bootstrap", func(c *cluster.Command) { c.Primary = a.cfg.InitialPrimary; c.TransitionID = ""; c.ExpectedPhase = "" })
	}
	if local.Database.Healthy {
		a.coldStart = false
	}
	if a.coldStart && s.Primary == a.cfg.ID && (s.Transition == nil || s.Transition.Phase == "complete") {
		return a.reconcileLocal(ctx, s, local)
	}
	if a.store.IsLeader() {
		for id, request := range s.Reinitializations {
			o, ok := a.sample(id)
			if ok && o.VerifiedRecoveryRequest == request.ID && request.Generation == s.Generation && o.Generation == s.Generation && o.Error == "" && o.Database.Healthy && o.Database.Recovery && o.Database.ReceiverStreaming && o.Database.ReceiverHost == a.primaryHost(s.Primary) && o.RecoveryState == "" && time.Since(o.Started) <= time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second {
				return a.command(ctx, s, "reinitialized", func(c *cluster.Command) { c.NodeID = id; c.TransitionID = request.ID })
			}
		}
		if s.Transition != nil && s.Transition.Phase != "complete" {
			return a.transition(ctx, s, local)
		}
		a.mu.RLock()
		failures := a.failures[s.Primary]
		a.mu.RUnlock()
		if failures >= a.cfg.FailureThreshold {
			return a.beginFailover(ctx, s)
		}
	}
	return a.reconcileLocal(ctx, s, local)
}
func (a *Runtime) candidate(ctx context.Context, s cluster.State, id string) (Evidence, error) {
	o, ok := a.sample(id)
	if !ok {
		return Evidence{}, errors.New("candidate agent observation unavailable")
	}
	if id == a.cfg.ID {
		started := time.Now()
		o = sample{a.localObservation(ctx), started}
	}
	if _, pending := s.Reinitializations[id]; pending {
		return Evidence{}, errors.New("candidate recovery is pending")
	}
	t, ok := a.targets[id]
	if !ok || o.ContainerID != t.ContainerID {
		return Evidence{}, errors.New("candidate not in current membership")
	}
	fenced, e := a.fence.IsFenced(ctx, t)
	if e != nil {
		return Evidence{}, e
	}
	if fenced {
		return Evidence{}, errors.New("candidate is fenced")
	}
	a.mu.RLock()
	p := a.primaryEvidence[s.Primary]
	a.mu.RUnlock()
	c := evidence(o)
	_, e = Eligible(c, p, s.Generation, a.cfg.MaxPromotionLagBytes, time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second, time.Now())
	return c, e
}
func (a *Runtime) beginFailover(ctx context.Context, s cluster.State) error {
	c, e := a.candidate(ctx, s, a.cfg.ID)
	if e != nil {
		nodes := append([]Node(nil), a.cfg.Nodes...)
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		for _, n := range nodes {
			if n.ID == a.cfg.ID || n.ID == s.Primary {
				continue
			}
			if _, ce := a.candidate(ctx, s, n.ID); ce == nil {
				return a.store.Transfer(ctx, n.ID)
			}
		}
		return fmt.Errorf("no eligible failover leader: %w", e)
	}
	return a.command(ctx, s, "begin", func(cmd *cluster.Command) {
		cmd.TransitionID = newID()
		cmd.Candidate = a.cfg.ID
		cmd.SystemID = c.SystemID
		cmd.Timeline = c.Timeline
		cmd.ReplayLSN = c.ReplayLSN
		cmd.OldContainerID = a.targets[s.Primary].ContainerID
		cmd.CandidateContainerID = c.ContainerID
	})
}
func (a *Runtime) transition(ctx context.Context, s cluster.State, local Observation) error {
	t := s.Transition
	if t.Candidate != a.cfg.ID {
		// A leadership change cannot remotely promote a database. Return leadership
		// to the authorized candidate; if it is unavailable, retain authority.
		o, ok := a.sample(t.Candidate)
		if !ok || candidateAgentReady(s, o, time.Now(), time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second) != nil {
			return errors.New("transition candidate unavailable; authority retained")
		}
		return a.store.Transfer(ctx, t.Candidate)
	}
	target := fencing.Target{ClusterID: a.cfg.ClusterID, NodeID: t.OldPrimary, ContainerID: t.OldContainerID}
	verifyFence := func() error {
		yes, e := a.fence.IsFenced(ctx, target)
		if e != nil {
			return e
		}
		if !yes {
			return errors.New("old primary isolation is not verified")
		}
		return a.fault(ctx, "after-fence-verification")
	}
	fence := func() error {
		if e := a.fault(ctx, "before-fence"); e != nil {
			return e
		}
		if e := a.fence.Fence(ctx, target); e != nil {
			return e
		}
		return a.fault(ctx, "after-fence")
	}
	switch t.Phase {
	case "validating":
		if _, e := a.candidate(ctx, s, a.cfg.ID); e != nil {
			return e
		}
		return a.command(ctx, s, "fencing", nil)
	case "fencing":
		if _, e := a.candidate(ctx, s, a.cfg.ID); e != nil {
			return e
		}
		if e := fence(); e != nil {
			return e
		}
		if e := verifyFence(); e != nil {
			return e
		}
		c, e := a.candidate(ctx, s, a.cfg.ID)
		if e != nil {
			return e
		}
		return a.command(ctx, s, "authorize", func(cmd *cluster.Command) {
			cmd.SystemID = c.SystemID
			cmd.Timeline = c.Timeline
			cmd.ReplayLSN = c.ReplayLSN
			cmd.FenceProof = t.ID + ":" + t.OldContainerID
		})
	case "authorized", "promoting":
		if s.Primary != a.cfg.ID {
			return errors.New("stale promotion authorization")
		}
		if e := fence(); e != nil {
			return e
		}
		if e := verifyFence(); e != nil {
			return e
		}
		if !local.Database.Healthy {
			if !a.pg.Exists() {
				return errors.New("authorized candidate database missing")
			}
			if local.RecoveryState != "" {
				return errors.New("authorized candidate recovery is incomplete")
			}
			if e := a.unchanged(ctx, s); e != nil {
				return e
			}
			if e := a.pg.RecoverAuthorizedPromotion(ctx, t.SystemID, t.Timeline, t.ReplayLSN); e != nil {
				return e
			}
			if e := a.unchanged(ctx, s); e != nil {
				return e
			}
			if !a.store.IsLeader() {
				return errors.New("leadership changed during promotion recovery")
			}
			if e := verifyFence(); e != nil {
				return e
			}
			if e := a.pg.Start(ctx); e != nil {
				return e
			}
			local = a.localObservation(ctx)
		}
		db := local.Database
		if local.Error != "" || !db.Healthy || db.SystemID != t.SystemID || db.ReplayPaused || local.ContainerID != t.CandidateContainerID || local.RecoveryState != "" {
			return errors.New("authorized candidate health or identity not verified")
		}
		if db.Recovery {
			if db.Timeline != t.Timeline || db.ReplayLSN < t.ReplayLSN {
				return errors.New("authorized candidate lost required replay history")
			}
			if t.Phase == "authorized" {
				return a.command(ctx, s, "promoting", nil)
			}
			if e := a.fault(ctx, "before-promote"); e != nil {
				return e
			}
			if e := a.unchanged(ctx, s); e != nil {
				return e
			}
			if !a.store.IsLeader() {
				return errors.New("leadership changed before promotion")
			}
			if e := verifyFence(); e != nil {
				return e
			}
			if e := a.pg.Promote(ctx); e != nil {
				return e
			}
			if e := a.fault(ctx, "after-promote"); e != nil {
				return e
			}
			local = a.localObservation(ctx)
			db = local.Database
		}
		if local.Error != "" || !db.Healthy || db.Recovery || db.SystemID != t.SystemID || db.Timeline <= t.Timeline || db.FlushLSN < t.ReplayLSN {
			return errors.New("promotion outcome not verified")
		}
		if e := a.pg.EnsurePrimary(ctx); e != nil {
			return e
		}
		// Record the intermediate phase after a crash immediately following promote.
		if t.Phase == "authorized" {
			return a.command(ctx, s, "promoting", nil)
		}
		return a.command(ctx, s, "reconfiguring", func(cmd *cluster.Command) { cmd.Timeline = db.Timeline })
	case "reconfiguring":
		if e := a.reconcileLocal(ctx, s, local); e != nil {
			return e
		}
		for _, n := range a.cfg.Nodes {
			if n.ID == s.Primary {
				continue
			}
			yes, e := a.fence.IsFenced(ctx, a.targets[n.ID])
			if e != nil {
				return e
			}
			if yes {
				continue
			}
			o, ok := a.sample(n.ID)
			host := a.primaryHost(s.Primary)
			if !ok || o.Error != "" || !o.Database.Healthy || !o.Database.Recovery || !o.Database.ReceiverStreaming || o.Database.ReplayPaused || o.Database.SystemID != t.SystemID || o.Database.ReplayLSN < t.ReplayLSN || o.Database.ReceiverHost != host || o.RecoveryState != "" || o.Generation != s.Generation || time.Since(o.Started) > time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second {
				return errors.New("waiting for remaining replica verification")
			}
		}
		return a.command(ctx, s, "complete", nil)
	default:
		return errors.New("unknown transition phase")
	}
}
func (a *Runtime) primaryHost(id string) string {
	n, _ := a.cfg.Node(id)
	host, _, _ := net.SplitHostPort(n.PostgresAddress)
	return host
}
func blocksRestart(s cluster.State, node string) bool {
	t := s.Transition
	return t != nil && t.OldPrimary == node && t.Phase != "reconfiguring" && t.Phase != "complete"
}
func candidateAgentReady(s cluster.State, o sample, now time.Time, maxAge time.Duration) error {
	t := s.Transition
	if t == nil || o.NodeID != t.Candidate || o.ContainerID != t.CandidateContainerID || o.Incarnation == "" || o.Generation != s.Generation || now.Sub(o.Started) > maxAge || o.Started.After(now) {
		return errors.New("candidate agent identity or freshness not verified")
	}
	if t.Phase != "authorized" && t.Phase != "promoting" && t.Phase != "reconfiguring" && (o.Error != "" || !o.Database.Healthy) {
		return errors.New("candidate database unavailable")
	}
	return nil
}
func (a *Runtime) reconcileLocal(ctx context.Context, s cluster.State, local Observation) error {
	if !local.Database.Healthy && blocksRestart(s, a.cfg.ID) {
		return errors.New("old primary startup blocked until replacement promotion is verified")
	}
	if s.Primary == a.cfg.ID {
		if local.Database.Healthy {
			if local.Database.Recovery {
				return errors.New("authorized primary in recovery without resumable promotion")
			}
			return a.pg.EnsurePrimary(ctx)
		}
		if !a.coldStart {
			return errors.New("primary PostgreSQL failed; waiting for coordinated failover")
		}
		if local.RecoveryState != "" {
			return errors.New("primary has incomplete recovery; refusing startup")
		}
		if local.Empty {
			if s.Generation != 1 || s.Transition != nil {
				return errors.New("cannot initialize empty authorized primary after failover")
			}
			if e := a.pg.InitializePrimary(ctx); e != nil {
				return e
			}
		}
		if _, e := os.Stat(filepath.Join(a.cfg.DataDir, "standby.signal")); e == nil {
			return errors.New("cannot start authorized primary from unexpected standby data")
		}
		if e := a.unchanged(ctx, s); e != nil {
			return e
		}
		if e := a.pg.Start(ctx); e != nil {
			return e
		}
		a.coldStart = false
		return nil
	}
	up, e := a.upstream(s)
	if e != nil {
		return e
	}
	if request, ok := s.Reinitializations[a.cfg.ID]; ok {
		if request.Generation != s.Generation {
			return errors.New("reinitialization request generation is stale")
		}
		if e = a.pg.Reinitialize(ctx, up, request.ID); e != nil {
			return e
		}
		if e = a.unchanged(ctx, s); e != nil {
			return e
		}
		if e = a.pg.Start(ctx); e != nil {
			return e
		}
		if e = a.pg.VerifyReplica(ctx, up); e != nil {
			return e
		}
		a.mu.Lock()
		a.verifiedRecovery = request.ID
		a.mu.Unlock()
		// Completion is committed by the leader after observing this replica; local
		// recovery remains excluded from promotion until that acknowledgement.
		return nil
	}
	if local.RecoveryState != "" {
		return fmt.Errorf("reinitialization_required: %s", local.RecoveryState)
	}
	if local.Database.Healthy && local.Database.Recovery {
		return a.pg.Follow(ctx, up)
	}
	if local.Empty {
		if e = a.pg.InitializeReplica(ctx, up); e != nil {
			return e
		}
	} else {
		if _, err := os.Lstat(filepath.Join(a.cfg.DataDir, "standby.signal")); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if e = a.pg.Rewind(ctx, up); e != nil {
				return e
			}
		} else {
			if e = a.pg.Follow(ctx, up); e != nil {
				return e
			}
		}
	}
	if e = a.unchanged(ctx, s); e != nil {
		return e
	}
	if e = a.pg.Start(ctx); e != nil {
		return e
	}
	return a.pg.VerifyReplica(ctx, up)
}
