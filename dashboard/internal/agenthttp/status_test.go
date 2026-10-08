package agenthttp

import (
	"encoding/json"
	"maat/dashboard/internal/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func response() map[string]any {
	return map[string]any{"node_id": "b", "raft_leader": "a", "raft_is_leader": false, "raft_term": 9,
		"observed_replication_lag_bytes": uint64(9007199254740993), "primary_observation_age_seconds": 0.2,
		"state": map[string]any{"ClusterID": "lab", "Primary": "a", "Generation": 4, "Nodes": []any{map[string]any{"ID": "b", "ContainerID": "opaque-b"}}},
		"local": map[string]any{"node_id": "b", "cluster_id": "lab", "container_id": "opaque-b", "database": map[string]any{"healthy": true, "recovery": true, "flush_lsn": uint64(9007199254740993), "replay_lsn": 12, "receiver_streaming": true, "timeline": 3, "received_timeline": 3, "replay_paused": false}}}
}
func TestReadValidatesIdentityAndPreservesIntegers(t *testing.T) {
	body := response()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	target := core.Target{ID: "b", ClusterID: "lab", InstanceID: "opaque-b", Endpoint: server.URL}
	s, err := New().Read(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if s.LagBytes == nil || *s.LagBytes != 9007199254740993 || s.FlushLSN != 9007199254740993 {
		t.Fatalf("precision lost: %+v", s)
	}
	target.InstanceID = "replacement"
	if _, err = New().Read(t.Context(), target); err == nil {
		t.Fatal("wrong identity accepted")
	}
}
func TestReadRejectsUntrustedTransportAndPayloads(t *testing.T) {
	for _, name := range []string{"oversized", "redirect", "missing-database", "missing-timeline", "non200", "trailing-json", "duplicate-membership", "unknown-member", "missing-membership", "wrong-local-node"} {
		t.Run(name, func(t *testing.T) {
			body := response()
			if name == "missing-timeline" {
				delete(body["local"].(map[string]any)["database"].(map[string]any), "timeline")
			}
			if name == "missing-database" {
				delete(body["local"].(map[string]any), "database")
			}
			if name == "duplicate-membership" {
				state := body["state"].(map[string]any)
				state["Nodes"] = append(state["Nodes"].([]any), state["Nodes"].([]any)[0])
			}
			if name == "unknown-member" {
				body["state"].(map[string]any)["Nodes"] = []any{map[string]any{"ID": "c", "ContainerID": "opaque-c"}}
			}
			if name == "missing-membership" {
				delete(body["state"].(map[string]any), "Nodes")
			}
			if name == "wrong-local-node" {
				body["local"].(map[string]any)["node_id"] = "a"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch name {
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", 1<<20+1)))
				case "redirect":
					http.Redirect(w, r, "http://example.com", http.StatusFound)
				case "non200":
					http.Error(w, "private diagnostic", http.StatusServiceUnavailable)
				default:
					_ = json.NewEncoder(w).Encode(body)
					if name == "trailing-json" {
						_, _ = w.Write([]byte("{}"))
					}
				}
			}))
			defer server.Close()
			_, err := New().Read(t.Context(), core.Target{ID: "b", ClusterID: "lab", InstanceID: "opaque-b", Endpoint: server.URL})
			if err == nil || strings.Contains(err.Error(), "private diagnostic") {
				t.Fatalf("unsafe response accepted: %v", err)
			}
		})
	}
	if _, err := New().Read(t.Context(), core.Target{Endpoint: "http://example.com"}); err == nil {
		t.Fatal("nonlocal endpoint accepted")
	}
}
