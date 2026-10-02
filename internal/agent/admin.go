package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"maat/internal/cluster"
)

type ReinitializeRequest struct {
	NodeID      string `json:"node_id"`
	Generation  uint64 `json:"generation"`
	Acknowledge bool   `json:"acknowledge_data_replacement"`
}

func (a *Runtime) adminServer(serveErrors chan<- error) (*http.Server, error) {
	path := filepath.Join(a.cfg.StateDir, "admin.sock")
	if st, e := os.Lstat(path); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("admin socket path is not a socket")
		}
		if c, e := net.DialTimeout("unix", path, time.Second); e == nil {
			c.Close()
			return nil, errors.New("another agent already owns the admin socket")
		}
		if e = os.Remove(path); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	listener, e := net.Listen("unix", path)
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		listener.Close()
		return nil, e
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", a.statusHTTP)
	mux.HandleFunc("POST /reinitialize", a.reinitializeHTTP)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: a.cfg.Timeout(), WriteTimeout: a.cfg.Timeout()}
	go func() { serveErrors <- server.Serve(listener) }()
	return server, nil
}
func validateReinitialize(s cluster.State, request ReinitializeRequest) error {
	if !request.Acknowledge || request.Generation == 0 || request.Generation != s.Generation {
		return errors.New("explicit data-replacement acknowledgement and current generation required")
	}
	if request.NodeID == s.Primary {
		return errors.New("cannot reinitialize the authorized primary")
	}
	found := false
	for _, n := range s.Nodes {
		found = found || n.ID == request.NodeID
	}
	if !found {
		return errors.New("target is not a cluster member")
	}
	if s.Transition != nil && s.Transition.Phase != "complete" {
		return errors.New("failover transition in progress")
	}
	return nil
}
func (a *Runtime) reinitializeHTTP(w http.ResponseWriter, r *http.Request) {
	var req ReinitializeRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if e := d.Decode(&req); e != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if e := d.Decode(new(any)); e != io.EOF {
		http.Error(w, "unexpected trailing request", 400)
		return
	}
	if !a.store.IsLeader() {
		http.Error(w, "run this local command on Raft leader "+a.store.LeaderID(), 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout())
	defer cancel()
	s, e := a.store.LinearizableState(ctx)
	if e != nil {
		http.Error(w, "quorum authority unavailable", 503)
		return
	}
	if e = validateReinitialize(s, req); e != nil {
		http.Error(w, e.Error(), 409)
		return
	}
	if pending, ok := s.Reinitializations[req.NodeID]; ok && pending.Generation == s.Generation {
		o, observed := a.sample(req.NodeID)
		if !observed || !failedRecovery(s, req.NodeID, pending.ID, o, time.Now(), time.Duration(a.cfg.MaxObservationAgeSeconds)*time.Second) {
			writeJSON(w, pending)
			return
		}
	}
	id := newID()
	e = a.command(ctx, s, "reinitialize", func(c *cluster.Command) { c.TransitionID = id; c.NodeID = req.NodeID })
	if e != nil {
		http.Error(w, "recovery request could not be committed", 503)
		return
	}
	writeJSON(w, map[string]string{"request_id": id, "status": "committed"})
}
func Control(ctx context.Context, c Config, path string, body any, out io.Writer) error {
	socket := filepath.Join(c.StateDir, "admin.sock")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: c.Timeout()}
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
		method = http.MethodPost
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://local"+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	response, e := client.Do(req)
	if e != nil {
		return errors.New("local agent unavailable; run the command inside the target node as postgres")
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if e != nil {
		return e
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("agent rejected request: %s", bytes.TrimSpace(b))
	}
	_, e = out.Write(b)
	return e
}

func failedRecovery(s cluster.State, node, requestID string, o sample, now time.Time, maxAge time.Duration) bool {
	for _, n := range s.Nodes {
		if n.ID == node {
			return o.NodeID == node && o.ContainerID == n.ContainerID && o.Incarnation != "" && o.Generation == s.Generation && o.RecoveryState == "reinitialization_required" && o.RecoveryRequestID == requestID && !o.Started.After(now) && now.Sub(o.Started) <= maxAge
		}
	}
	return false
}
