// Package core owns dashboard state without deployment or terminal dependencies.
package core

import (
	"context"
	"errors"
	"maps"
	"math"
	"sort"
	"sync"
	"time"
)

var ErrIdentity = errors.New("status identity conflict")

type Target struct {
	ID, ClusterID, InstanceID, Endpoint, Runtime, Error string
	Available                                           bool
}

type Status struct {
	Members                                                 map[string]string
	ID, ClusterID, InstanceID                               string
	Healthy, Recovery, ReceiverStreaming, ReplayPaused      bool
	SystemID, ReceiverHost, RecoveryState, ObservationError string
	Timeline, ReceivedTimeline, FlushLSN, ReplayLSN         uint64
	Generation, Term                                        uint64
	Primary, Leader                                         string
	IsLeader                                                bool
	LagBytes                                                *uint64
	SampleAge                                               *float64
	QuorumAt, ReconcileAt                                   time.Time
	ReconcileError, Transition                              string
}

type Discovery interface {
	Discover(context.Context) ([]Target, error)
}
type StatusReader interface {
	Read(context.Context, Target) (Status, error)
}
type SessionRunner interface {
	Run(context.Context, Target) error
}

type Node struct {
	Target           Target
	Status           *Status
	Received         time.Time
	Error            string
	Bound            *Target
	IdentityConflict bool
}
type View struct {
	Nodes           []Node
	Selected, Error string
}
type Measurement struct {
	Bytes *uint64
	Age   *float64
	State string
}

type App struct {
	discovery Discovery
	reader    StatusReader
	session   SessionRunner
	mu        sync.RWMutex
	refreshMu sync.Mutex
	view      View
	epoch     uint64
	paused    bool
	cancel    context.CancelFunc
}

func New(d Discovery, r StatusReader, s SessionRunner) *App {
	return &App{discovery: d, reader: r, session: s}
}

func same(a, b Target) bool {
	return a.ID == b.ID && a.ClusterID == b.ClusterID && a.InstanceID == b.InstanceID
}

func (a *App) Refresh(ctx context.Context) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	a.mu.Lock()
	if a.paused {
		a.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	epoch := a.epoch
	a.mu.Unlock()
	defer cancel()
	targets, err := a.discovery.Discover(ctx)
	a.mu.Lock()
	if a.paused || a.epoch != epoch {
		a.mu.Unlock()
		return nil
	}
	if err != nil {
		a.view.Error = err.Error()
		for i := range a.view.Nodes {
			a.view.Nodes[i].Error = "discovery unavailable"
			a.view.Nodes[i].Target.Available = false
			a.view.Nodes[i].Target.Runtime = "unknown"
		}
		a.mu.Unlock()
		return err
	}
	old := map[string]Node{}
	for _, n := range a.view.Nodes {
		old[n.Target.ID] = n
	}
	a.view.Error = ""
	a.view.Nodes = nil
	targets = append([]Target(nil), targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	for _, target := range targets {
		n := old[target.ID]
		n.Target = target
		if target.Error != "" {
			n.Error = target.Error
		}
		if n.Bound != nil && !same(*n.Bound, target) {
			n.Error = "instance identity changed"
			n.Target.Available = false
		}
		if !n.Target.Available && n.Error == "" {
			n.Error = "instance unavailable"
		}
		a.view.Nodes = append(a.view.Nodes, n)
	}
	if a.view.Selected == "" && len(a.view.Nodes) > 0 {
		a.view.Selected = a.view.Nodes[0].Target.ID
	}
	eligible := append([]Node(nil), a.view.Nodes...)
	a.mu.Unlock()
	identities := make(map[string]string, len(eligible))
	for _, n := range eligible {
		identities[n.Target.ID] = n.Target.InstanceID
		if n.Bound != nil {
			identities[n.Target.ID] = n.Bound.InstanceID
		}
	}
	var wg sync.WaitGroup
	for _, node := range eligible {
		if !node.Target.Available || node.Target.Error != "" {
			continue
		}
		wg.Go(func() {
			status, readErr := a.reader.Read(ctx, node.Target)
			if readErr == nil && (status.ID != node.Target.ID || status.ClusterID != node.Target.ClusterID || status.InstanceID != node.Target.InstanceID) {
				readErr = ErrIdentity
			}
			if readErr == nil {
				if len(status.Members) != len(identities) {
					readErr = ErrIdentity
				}
				for id, instance := range status.Members {
					expected, exists := identities[id]
					if !exists || instance == "" || (expected != "" && instance != expected) {
						readErr = ErrIdentity
					}
				}
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.paused || a.epoch != epoch {
				return
			}
			for i := range a.view.Nodes {
				n := &a.view.Nodes[i]
				if n.Target.ID != node.Target.ID {
					continue
				}
				if readErr != nil {
					n.Error = readErr.Error()
					n.IdentityConflict = n.IdentityConflict || errors.Is(readErr, ErrIdentity)
					return
				}
				n.IdentityConflict = false
				n.Status = &status
				n.Received = time.Now()
				n.Error = ""
				if n.Bound == nil {
					bound := node.Target
					n.Bound = &bound
				}
				return
			}
		})
	}
	wg.Wait()
	return nil
}

func (a *App) Snapshot() View {
	a.mu.RLock()
	defer a.mu.RUnlock()
	v := a.view
	v.Nodes = append([]Node(nil), v.Nodes...)
	for i := range v.Nodes {
		n := &v.Nodes[i]
		if n.Status != nil {
			s := *n.Status
			s.Members = maps.Clone(s.Members)
			n.Status = &s
		}
		if n.Bound != nil {
			b := *n.Bound
			n.Bound = &b
		}
	}
	return v
}
func (a *App) Move(delta int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := len(a.view.Nodes)
	if count == 0 {
		return
	}
	for i, n := range a.view.Nodes {
		if n.Target.ID == a.view.Selected {
			a.view.Selected = a.view.Nodes[((i+delta)%count+count)%count].Target.ID
			return
		}
	}
	a.view.Selected = a.view.Nodes[0].Target.ID
}
func (a *App) Pause() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paused = true
	a.epoch++
	if a.cancel != nil {
		a.cancel()
	}
	for i := range a.view.Nodes {
		a.view.Nodes[i].Error = "refresh required after session"
	}
}
func (a *App) Resume() { a.mu.Lock(); a.paused = false; a.mu.Unlock() }

// Poll serializes refresh cycles; request asks for a refresh after a session.
func (a *App) Poll(ctx context.Context, request <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-request:
		}
		if ctx.Err() != nil {
			return
		}
		_ = a.Refresh(ctx)
	}
}
func (a *App) OpenSession(ctx context.Context) error {
	v := a.Snapshot()
	for _, n := range v.Nodes {
		if n.Target.ID == v.Selected {
			if n.IdentityConflict || n.Bound == nil || !same(*n.Bound, n.Target) {
				return errors.New("node has not been validated or its identity changed")
			}
			if !n.Target.Available {
				return errors.New("instance is unavailable")
			}
			return a.session.Run(ctx, *n.Bound)
		}
	}
	return errors.New("no selected node")
}
func (n Node) Lag(now time.Time) Measurement {
	m := Measurement{State: "unknown"}
	if n.Status == nil {
		return m
	}
	if n.Status.Healthy && !n.Status.Recovery {
		m.State = "n/a"
		return m
	}
	m.Bytes = n.Status.LagBytes
	if n.Status.SampleAge != nil && *n.Status.SampleAge >= 0 && !math.IsNaN(*n.Status.SampleAge) && !math.IsInf(*n.Status.SampleAge, 0) {
		age := *n.Status.SampleAge + math.Max(0, now.Sub(n.Received).Seconds())
		m.Age = &age
	}
	if m.Bytes != nil && m.Age != nil {
		m.State = "current"
		if *m.Age > 30 || n.Error != "" {
			m.State = "stale"
		}
	}
	return m
}
func (v View) Consensus() (Status, int, bool) {
	var first Status
	count := 0
	agree := true
	for _, n := range v.Nodes {
		if n.Status == nil || n.Error != "" {
			continue
		}
		s := *n.Status
		if count == 0 {
			first = s
		} else if s.Generation != first.Generation || s.Primary != first.Primary || s.Leader != first.Leader {
			agree = false
		}
		count++
		if s.Generation == 0 || s.Primary == "" || s.Leader == "" {
			agree = false
		}
	}
	return first, count, agree && count > 0
}
