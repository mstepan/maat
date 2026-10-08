// Package agenthttp maps the agent HTTP contract to deployment-independent observations.
package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maat/dashboard/internal/core"
	"net"
	"net/http"
	"net/url"
	"time"
)

type Reader struct{ client *http.Client }

func New() *Reader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Reader{client: &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type wire struct {
	ID             string    `json:"node_id"`
	Leader         string    `json:"raft_leader"`
	IsLeader       *bool     `json:"raft_is_leader"`
	Term           *uint64   `json:"raft_term"`
	Lag            *uint64   `json:"observed_replication_lag_bytes"`
	Age            *float64  `json:"primary_observation_age_seconds"`
	QuorumAt       time.Time `json:"quorum_last_confirmed_at"`
	ReconcileAt    time.Time `json:"last_successful_reconcile"`
	ReconcileError string    `json:"reconciliation_error"`
	State          struct {
		ClusterID, Primary string
		Generation         *uint64
		Nodes              []struct{ ID, ContainerID string }
		Transition         *struct{ Phase string }
	} `json:"state"`
	Local struct {
		ID            string `json:"node_id"`
		ClusterID     string `json:"cluster_id"`
		InstanceID    string `json:"container_id"`
		RecoveryState string `json:"recovery_state"`
		Error         string `json:"error"`
		Database      *struct {
			Healthy           *bool   `json:"healthy"`
			Recovery          *bool   `json:"recovery"`
			SystemID          string  `json:"system_id"`
			Timeline          *uint64 `json:"timeline"`
			FlushLSN          *uint64 `json:"flush_lsn"`
			ReplayLSN         *uint64 `json:"replay_lsn"`
			ReceiverStreaming *bool   `json:"receiver_streaming"`
			ReceiverHost      string  `json:"receiver_host"`
			ReceivedTimeline  *uint64 `json:"received_timeline"`
			ReplayPaused      *bool   `json:"replay_paused"`
		} `json:"database"`
	} `json:"local"`
}

func (r *Reader) Read(ctx context.Context, t core.Target) (core.Status, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Port() == "" || !net.ParseIP(u.Hostname()).IsLoopback() {
		return core.Status{}, errors.New("status endpoint must be explicit loopback HTTP")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Endpoint+"/status", nil)
	if err != nil {
		return core.Status{}, errors.New("invalid status endpoint")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return core.Status{}, errors.New("status request unavailable or timed out")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return core.Status{}, fmt.Errorf("status returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return core.Status{}, errors.New("status response unreadable or exceeds 1 MiB")
	}
	var w wire
	if err = json.Unmarshal(body, &w); err != nil {
		return core.Status{}, errors.New("invalid status JSON")
	}
	if t.ID == "" || t.ClusterID == "" || t.InstanceID == "" || w.ID != t.ID || w.Local.ID != t.ID || w.Local.ClusterID != t.ClusterID || w.State.ClusterID != t.ClusterID || w.Local.InstanceID != t.InstanceID {
		return core.Status{}, fmt.Errorf("status identity mismatch: %w", core.ErrIdentity)
	}
	seen := map[string]bool{}
	members := map[string]string{}
	matched := false
	for _, member := range w.State.Nodes {
		if member.ID == "" || member.ContainerID == "" || seen[member.ID] {
			return core.Status{}, fmt.Errorf("invalid status membership: %w", core.ErrIdentity)
		}
		seen[member.ID] = true
		members[member.ID] = member.ContainerID
		if member.ID == t.ID {
			if member.ContainerID != t.InstanceID {
				return core.Status{}, fmt.Errorf("membership instance mismatch: %w", core.ErrIdentity)
			}
			matched = true
		}
	}
	if !matched {
		return core.Status{}, fmt.Errorf("node missing from status membership: %w", core.ErrIdentity)
	}
	d := w.Local.Database
	if d == nil || d.Healthy == nil || d.Recovery == nil || w.IsLeader == nil || w.Term == nil || w.State.Generation == nil || d.Timeline == nil || d.FlushLSN == nil || d.ReplayLSN == nil || d.ReceivedTimeline == nil || d.ReceiverStreaming == nil || d.ReplayPaused == nil {
		return core.Status{}, errors.New("missing required status fields")
	}
	transition := "none reported"
	if w.State.Transition != nil {
		transition = w.State.Transition.Phase
	}
	return core.Status{Members: members, ID: w.ID, ClusterID: w.Local.ClusterID, InstanceID: w.Local.InstanceID, Healthy: *d.Healthy, Recovery: *d.Recovery, SystemID: d.SystemID, Timeline: *d.Timeline, FlushLSN: *d.FlushLSN, ReplayLSN: *d.ReplayLSN, ReceiverStreaming: *d.ReceiverStreaming, ReceiverHost: d.ReceiverHost, ReceivedTimeline: *d.ReceivedTimeline, ReplayPaused: *d.ReplayPaused, Generation: *w.State.Generation, Primary: w.State.Primary, Leader: w.Leader, IsLeader: *w.IsLeader, Term: *w.Term, LagBytes: w.Lag, SampleAge: w.Age, QuorumAt: w.QuorumAt, ReconcileAt: w.ReconcileAt, ReconcileError: w.ReconcileError, RecoveryState: w.Local.RecoveryState, ObservationError: w.Local.Error, Transition: transition}, nil
}
