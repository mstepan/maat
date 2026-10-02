package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"maat/internal/cluster"
	"maat/internal/fencing"
	"maat/internal/postgres"
)

type Observation struct {
	NodeID                  string               `json:"node_id"`
	ClusterID               string               `json:"cluster_id"`
	ContainerID             string               `json:"container_id"`
	Incarnation             string               `json:"incarnation"`
	Generation              uint64               `json:"generation"`
	Database                postgres.Observation `json:"database"`
	Empty                   bool                 `json:"empty"`
	RecoveryRequestID       string               `json:"recovery_request_id,omitempty"`
	RecoveryState           string               `json:"recovery_state,omitempty"`
	VerifiedRecoveryRequest string               `json:"verified_recovery_request,omitempty"`
	Error                   string               `json:"error,omitempty"`
}
type sample struct {
	Observation
	Started time.Time
}
type Runtime struct {
	cfg              Config
	store            *cluster.Store
	pg               *postgres.Controller
	fence            fencing.Fencer
	docker           *fencing.Docker
	targets          map[string]fencing.Target
	incarnation      string
	http             *http.Client
	mu               sync.RWMutex
	samples          map[string]sample
	primaryEvidence  map[string]Evidence
	failures         map[string]int
	lastError        string
	lastSuccess      time.Time
	coldStart        bool
	verifiedRecovery string
	lastAuthority    time.Time
}

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Run(ctx context.Context, c Config) error {
	node, _ := c.Node(c.ID)
	_, port, _ := net.SplitHostPort(node.PostgresAddress)
	p, _ := strconv.Atoi(port)
	pg, e := postgres.New(postgres.Config{DataDir: c.DataDir, StateDir: filepath.Join(c.StateDir, "postgres"), PasswordFile: c.PasswordFile, ReplicationPasswordFile: c.ReplicationPasswordFile, NodeID: c.ID, Port: uint16(p), Peers: []string{c.Nodes[0].ID, c.Nodes[1].ID, c.Nodes[2].ID}})
	if e != nil {
		return e
	}
	// Never recreate consensus authority around surviving PostgreSQL data.
	empty, e := pg.Empty()
	if e != nil {
		return e
	}
	raftDir := filepath.Join(c.StateDir, "raft")
	entries, e := os.ReadDir(raftDir)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if !empty && len(entries) == 0 {
		return errors.New("database data exists without Raft state; operator recovery required")
	}
	d, e := fencing.NewDocker(c.DockerSocket, c.Timeout())
	if e != nil {
		return e
	}
	targets := map[string]fencing.Target{}
	nodes := make([]cluster.Node, 0, 3)
	for _, n := range c.Nodes {
		var target fencing.Target
		for {
			target, e = d.Resolve(ctx, c.ClusterID, n.ID)
			if e == nil {
				break
			}
			slog.Warn("waiting for managed Docker identity", "node", n.ID, "error", e)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		targets[n.ID] = target
		nodes = append(nodes, cluster.Node{ID: n.ID, RaftAddress: n.RaftAddress, HTTPAddress: n.HTTPAddress, PostgresAddress: n.PostgresAddress, ContainerID: target.ContainerID})
	}
	store, e := cluster.Start(cluster.Config{ID: c.ID, ClusterID: c.ClusterID, Bind: node.RaftAddress, Dir: raftDir, Nodes: nodes, Bootstrap: c.ID == c.BootstrapSeed})
	if e != nil {
		return e
	}
	defer store.Close()
	a := &Runtime{cfg: c, store: store, pg: pg, fence: d, docker: d, targets: targets, incarnation: newID(), http: &http.Client{Timeout: c.Timeout()}, samples: map[string]sample{}, primaryEvidence: map[string]Evidence{}, failures: map[string]int{}, coldStart: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /observation", a.observeHTTP)
	mux.HandleFunc("GET /status", a.statusHTTP)
	mux.HandleFunc("GET /authority", a.authorityHTTP)
	server := &http.Server{Addr: node.HTTPAddress, Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: c.Timeout(), WriteTimeout: c.Timeout(), IdleTimeout: 30 * time.Second}
	listen, e := net.Listen("tcp", node.HTTPAddress)
	if e != nil {
		return e
	}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- server.Serve(listen) }()
	defer server.Close()
	admin, e := a.adminServer(serveErrors)
	if e != nil {
		return e
	}
	defer admin.Close()
	tick := time.NewTicker(time.Duration(c.ObservationIntervalSeconds) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-serveErrors:
			if !errors.Is(e, http.ErrServerClosed) {
				return e
			}
		case <-tick.C:
			a.poll(ctx)
			stepCtx, cancel := context.WithTimeout(ctx, time.Duration(c.RecoveryTimeoutSeconds)*time.Second)
			e = a.reconcile(stepCtx)
			cancel()
			a.mu.Lock()
			if e != nil {
				if a.lastError != e.Error() {
					slog.Warn("reconciliation blocked", "node", c.ID, "reason", e)
				}
				a.lastError = e.Error()
			} else {
				a.lastError = ""
				a.lastSuccess = time.Now()
			}
			a.mu.Unlock()
		}
	}
}
func (a *Runtime) localObservation(ctx context.Context) Observation {
	s := a.store.State()
	o := Observation{NodeID: a.cfg.ID, ClusterID: a.cfg.ClusterID, ContainerID: a.targets[a.cfg.ID].ContainerID, Incarnation: a.incarnation, Generation: s.Generation}
	a.mu.RLock()
	o.VerifiedRecoveryRequest = a.verifiedRecovery
	a.mu.RUnlock()
	var e error
	o.Empty, e = a.pg.Empty()
	if e != nil {
		o.Error = "cannot inspect managed database directory"
		return o
	}
	o.RecoveryState, e = a.pg.RecoveryState()
	if e != nil {
		o.Error = "cannot read recovery state"
		return o
	}
	o.RecoveryRequestID, e = a.pg.RecoveryRequestID()
	if e != nil {
		o.Error = "cannot read recovery request"
		return o
	}
	o.Database, e = a.pg.Observe(ctx)
	if e != nil {
		o.Error = "PostgreSQL observation unavailable"
	}
	return o
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (a *Runtime) observeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	writeJSON(w, a.localObservation(ctx))
}
func (a *Runtime) statusHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	reason, success := a.lastError, a.lastSuccess
	quorumAt := a.lastAuthority
	evidence := a.primaryEvidence[a.store.State().Primary]
	a.mu.RUnlock()
	local := a.localObservation(r.Context())
	lag := any(nil)
	if local.Database.Healthy && local.Database.Recovery && local.Database.SystemID == evidence.SystemID && local.Database.Timeline == evidence.Timeline && !evidence.ReceivedAt.IsZero() {
		value := uint64(0)
		if evidence.FlushLSN > local.Database.ReplayLSN {
			value = evidence.FlushLSN - local.Database.ReplayLSN
		}
		lag = value
	}
	age := any(nil)
	if !evidence.ReceivedAt.IsZero() {
		age = time.Since(evidence.ReceivedAt).Seconds()
	}
	writeJSON(w, map[string]any{"node_id": a.cfg.ID, "agent_pid": os.Getpid(), "quorum_last_confirmed_at": quorumAt, "raft_leader": a.store.LeaderID(), "raft_is_leader": a.store.IsLeader(), "raft_term": a.store.Term(), "state": a.store.State(), "local": local, "observed_replication_lag_bytes": lag, "reconciliation_error": reason, "last_successful_reconcile": success, "primary_observation_age_seconds": age, "max_promotion_lag_bytes": a.cfg.MaxPromotionLagBytes, "data_loss": "asynchronous replication; final primary WAL and actual transaction loss may be unknown"})
}
func (a *Runtime) authorityHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout())
	defer cancel()
	s, e := a.store.LinearizableState(ctx)
	if e != nil {
		http.Error(w, "current quorum authority unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, s)
}
func (a *Runtime) get(ctx context.Context, address, path string, out any) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if e != nil {
		return e
	}
	resp, e := a.http.Do(req)
	if e != nil {
		return errors.New("agent request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent request returned status %d", resp.StatusCode)
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024))
	if e = d.Decode(out); e != nil {
		return errors.New("invalid agent response")
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return errors.New("invalid trailing agent response")
	}
	return nil
}
func (a *Runtime) authority(ctx context.Context) (cluster.State, error) {
	if a.store.IsLeader() {
		s, e := a.store.LinearizableState(ctx)
		if e == nil {
			a.mu.Lock()
			a.lastAuthority = time.Now()
			a.mu.Unlock()
		}
		return s, e
	}
	leader := a.store.LeaderID()
	n, ok := a.cfg.Node(leader)
	if !ok {
		return cluster.State{}, errors.New("Raft leader unknown")
	}
	var s cluster.State
	e := a.get(ctx, n.HTTPAddress, "/authority", &s)
	if e == nil && (s.ClusterID != a.cfg.ClusterID || s.Version != cluster.Version || len(s.Nodes) != 3) {
		e = errors.New("authority identity mismatch")
	}
	if e == nil {
		for _, n := range s.Nodes {
			t, ok := a.targets[n.ID]
			if !ok || n.ContainerID != t.ContainerID {
				e = errors.New("authority container identity mismatch")
				break
			}
		}
	}
	if e == nil {
		a.mu.Lock()
		a.lastAuthority = time.Now()
		a.mu.Unlock()
	}
	return s, e
}
func (a *Runtime) poll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, node := range a.cfg.Nodes {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			started := time.Now()
			pollCtx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.ObservationIntervalSeconds)*time.Second)
			defer cancel()
			var o Observation
			e := a.get(pollCtx, n.HTTPAddress, "/observation", &o)
			a.mu.Lock()
			defer a.mu.Unlock()
			if e != nil || o.NodeID != n.ID || o.ClusterID != a.cfg.ClusterID || o.ContainerID != a.targets[n.ID].ContainerID || o.Incarnation == "" {
				delete(a.samples, n.ID)
				a.failures[n.ID]++
				return
			}
			if old, ok := a.primaryEvidence[n.ID]; ok && (old.Incarnation != o.Incarnation || old.Generation != o.Generation || (o.Database.SystemID != "" && old.SystemID != o.Database.SystemID)) {
				delete(a.primaryEvidence, n.ID)
			}
			a.samples[n.ID] = sample{o, started}
			if o.Error == "" && o.Database.Healthy && !o.Database.Recovery {
				a.primaryEvidence[n.ID] = evidence(sample{o, started})
				a.failures[n.ID] = 0
			} else {
				a.failures[n.ID]++
			}
		}(node)
	}
	wg.Wait()
}
func evidence(s sample) Evidence {
	return Evidence{NodeID: s.NodeID, ContainerID: s.ContainerID, Incarnation: s.Incarnation, SystemID: s.Database.SystemID, RecoveryState: s.RecoveryState, Generation: s.Generation, Timeline: s.Database.Timeline, FlushLSN: s.Database.FlushLSN, ReplayLSN: s.Database.ReplayLSN, Healthy: s.Error == "" && s.Database.Healthy, Recovery: s.Database.Recovery, ReplayPaused: s.Database.ReplayPaused, ReceivedAt: s.Started}
}
func (a *Runtime) sample(id string) (sample, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.samples[id]
	return s, ok
}
func (a *Runtime) upstream(s cluster.State) (postgres.Upstream, error) {
	n, ok := a.cfg.Node(s.Primary)
	if !ok {
		return postgres.Upstream{}, errors.New("no authorized primary")
	}
	host, port, e := net.SplitHostPort(n.PostgresAddress)
	if e != nil {
		return postgres.Upstream{}, e
	}
	p, _ := strconv.Atoi(port)
	obs, ok := a.sample(s.Primary)
	if !ok || obs.Error != "" || !obs.Database.Healthy || obs.Database.Recovery || time.Since(obs.Started) > time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second || obs.Generation != s.Generation {
		return postgres.Upstream{}, errors.New("authorized primary is not freshly verified writable")
	}
	return postgres.Upstream{Host: host, Port: uint16(p), NodeID: n.ID, SystemID: obs.Database.SystemID}, nil
}
func (a *Runtime) unchanged(ctx context.Context, s cluster.State) error {
	current, e := a.authority(ctx)
	if e != nil {
		return e
	}
	if current.Generation != s.Generation || current.Primary != s.Primary {
		return errors.New("authority changed during database operation")
	}
	return nil
}
