package fencing

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const containerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const replacementID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func target() Target { return Target{ClusterID: "lab", NodeID: "a", ContainerID: containerID} }

type daemon struct {
	mu                               sync.Mutex
	running, restarting, paused      bool
	policy, inspectID, cluster, node string
	ids                              []string
	version, minimum                 string
	failPath                         string
	status                           int
	delay                            time.Duration
	stopLeavesRunning                bool
	stops, inspections               int
}

func newDaemon(t *testing.T) (*Docker, *daemon) {
	t.Helper()
	f := &daemon{running: true, policy: "no", inspectID: containerID, cluster: "lab", node: "a", ids: []string{containerID}, version: "1.54", minimum: "1.44"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		if f.failPath != "" && strings.Contains(r.URL.Path, f.failPath) {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte("secret-daemon-message"))
			return
		}
		if r.URL.Path == "/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": f.version, "MinAPIVersion": f.minimum})
			return
		}
		prefix := "/v1.54"
		if f.version == "1.44" {
			prefix = "/v1.44"
		}
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("unexpected API version path: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		switch path {
		case "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Error("listing must include stopped containers")
			}
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil || len(filters["label"]) != 2 || filters["label"][0] != "maat.cluster=lab" || filters["label"][1] != "maat.node=a" {
				t.Errorf("incorrect filters: %s", r.URL.RawQuery)
			}
			rows := make([]map[string]any, 0, len(f.ids))
			for _, id := range f.ids {
				rows = append(rows, map[string]any{"Id": id, "Labels": map[string]string{"maat.cluster": f.cluster, "maat.node": f.node}})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/containers/" + containerID + "/json":
			f.inspections++
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": f.inspectID, "Config": map[string]any{"Labels": map[string]string{"maat.cluster": f.cluster, "maat.node": f.node}}, "HostConfig": map[string]any{"RestartPolicy": map[string]string{"Name": f.policy}}, "State": map[string]bool{"Running": f.running, "Restarting": f.restarting, "Paused": f.paused}})
		case "/containers/" + containerID + "/stop":
			if r.Method != http.MethodPost || r.URL.Query().Get("t") == "" {
				t.Error("stop must be a bounded POST")
			}
			f.stops++
			if !f.stopLeavesRunning {
				f.running = false
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path: %s", path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	d, err := NewDocker("/var/run/docker.sock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d.client = server.Client()
	d.base = server.URL
	return d, f
}

func TestStopIsSeparateFromVerification(t *testing.T) {
	d, f := newDaemon(t)
	got, err := d.Resolve(context.Background(), "lab", "a")
	if err != nil || got != target() {
		t.Fatalf("resolve = %#v, %v", got, err)
	}
	if err := d.Fence(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	before := f.inspections
	ok, err := d.IsFenced(context.Background(), got)
	if err != nil || !ok || f.stops != 1 || f.inspections <= before {
		t.Fatalf("verification = %v, %v; stops %d inspections %d", ok, err, f.stops, f.inspections)
	}
	f.running = true
	if ok, err := d.IsFenced(context.Background(), got); err != nil || ok {
		t.Fatalf("restarted container fenced: %v, %v", ok, err)
	}
}

func TestStopSuccessDoesNotProveIsolation(t *testing.T) {
	d, f := newDaemon(t)
	f.stopLeavesRunning = true
	if err := d.Fence(context.Background(), target()); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.IsFenced(context.Background(), target()); err != nil || ok {
		t.Fatalf("still-running target fenced: %v, %v", ok, err)
	}
}

func TestUnsafeIdentityBlocksFenceAndVerification(t *testing.T) {
	tests := []struct {
		name  string
		alter func(*daemon)
	}{
		{"wrong cluster", func(f *daemon) { f.cluster = "other" }},
		{"wrong node", func(f *daemon) { f.node = "other" }},
		{"wrong inspect id", func(f *daemon) { f.inspectID = replacementID }},
		{"replacement", func(f *daemon) { f.ids = []string{replacementID} }},
		{"ambiguous", func(f *daemon) { f.ids = append(f.ids, replacementID) }},
		{"missing", func(f *daemon) { f.ids = nil }},
		{"restart policy", func(f *daemon) { f.policy = "always" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newDaemon(t)
			tc.alter(f)
			if err := d.Fence(context.Background(), target()); err == nil {
				t.Error("unsafe fence accepted")
			}
			if ok, err := d.IsFenced(context.Background(), target()); ok || err == nil {
				t.Fatalf("unsafe verification: %v, %v", ok, err)
			}
			if f.stops != 0 {
				t.Fatal("sent stop to unsafe target")
			}
		})
	}
}

func TestNonStoppedStatesNeverVerify(t *testing.T) {
	for _, state := range []string{"running", "restarting", "paused"} {
		t.Run(state, func(t *testing.T) {
			d, f := newDaemon(t)
			f.running = state == "running"
			f.restarting = state == "restarting"
			f.paused = state == "paused"
			if ok, err := d.IsFenced(context.Background(), target()); ok || err != nil {
				t.Fatalf("verification: %v, %v", ok, err)
			}
		})
	}
}

func TestDockerErrorsAreNotFencingEvidence(t *testing.T) {
	for _, path := range []string{"/version", "/containers/json", "/json", "/stop"} {
		for _, code := range []int{404, 403, 500} {
			t.Run(path+http.StatusText(code), func(t *testing.T) {
				d, f := newDaemon(t)
				f.failPath = path
				f.status = code
				err := d.Fence(context.Background(), target())
				if err == nil || strings.Contains(err.Error(), "secret-daemon-message") {
					t.Fatalf("error not safely handled: %v", err)
				}
				if path != "/stop" {
					if ok, err := d.IsFenced(context.Background(), target()); ok || err == nil {
						t.Fatalf("verification: %v, %v", ok, err)
					}
				}
			})
		}
	}
}

func TestDockerDeadline(t *testing.T) {
	d, f := newDaemon(t)
	f.delay = time.Second
	d.timeout = 20 * time.Millisecond
	started := time.Now()
	if ok, err := d.IsFenced(context.Background(), target()); ok || err == nil {
		t.Fatalf("timeout: %v %v", ok, err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("operation exceeded deadline")
	}
}

func TestDockerAPIVersionNegotiation(t *testing.T) {
	for _, tc := range []struct {
		max, min string
		want     bool
	}{
		{"1.54", "1.44", true}, {"1.44", "1.24", true}, {"1.60", "1.44", true},
		{"1.43", "1.24", false}, {"1.60", "1.55", false}, {"invalid", "1.44", false}, {"1.54", "", false}, {"1.44", "1.54", false},
	} {
		t.Run(tc.max+"-"+tc.min, func(t *testing.T) {
			d, f := newDaemon(t)
			f.version = tc.max
			f.minimum = tc.min
			_, err := d.Resolve(context.Background(), "lab", "a")
			if (err == nil) != tc.want {
				t.Fatalf("negotiation: %v", err)
			}
		})
	}
}

func TestEmptyRestartPolicyMeansNoRestart(t *testing.T) {
	d, f := newDaemon(t)
	f.running = false
	f.policy = ""
	if ok, err := d.IsFenced(context.Background(), target()); !ok || err != nil {
		t.Fatalf("empty policy: %v %v", ok, err)
	}
}

func TestResolveRequiresKnownStableIdentity(t *testing.T) {
	for _, state := range []string{"stopped", "paused", "restarting"} {
		t.Run(state, func(t *testing.T) {
			d, f := newDaemon(t)
			f.running = false
			f.paused = state == "paused"
			f.restarting = state == "restarting"
			got, err := d.Resolve(context.Background(), "lab", "a")
			if state == "stopped" {
				if err != nil || got != target() {
					t.Fatalf("stopped identity: %v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("unstable identity accepted")
			}
		})
	}
}

func TestConstructorRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		socket  string
		timeout time.Duration
	}{{"", time.Second}, {"relative", time.Second}, {"/socket", 0}, {"/socket", -time.Second}, {"/bad\x00", time.Second}} {
		if _, err := NewDocker(tc.socket, tc.timeout); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestCallerCancellationStopsRequests(t *testing.T) {
	d, _ := newDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Fence(ctx, target()); err == nil {
		t.Fatal("canceled operation succeeded")
	}
	if ok, err := d.IsFenced(ctx, target()); ok || err == nil {
		t.Fatalf("canceled verification: %v %v", ok, err)
	}
}

func TestInvalidTargetNeverReachesDaemon(t *testing.T) {
	d, _ := newDaemon(t)
	for _, bad := range []Target{{}, {ClusterID: "lab", NodeID: "a", ContainerID: "a"}, {ClusterID: "lab", NodeID: "a", ContainerID: strings.Repeat("g", 64)}, {ClusterID: "lab", NodeID: "a", ContainerID: "../../containers"}} {
		if err := d.Fence(context.Background(), bad); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}

func TestMalformedInspectionCannotProveFencing(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"Id":"` + containerID + `","Config":{"Labels":{"maat.cluster":"lab","maat.node":"a"}},"HostConfig":{"RestartPolicy":{"Name":"no"}},"State":{}}`, `{"secret":"invalid`, strings.Repeat(" ", (1<<20)+1)} {
		t.Run("malformed", func(t *testing.T) {
			d, _ := newDaemon(t)
			prior := d.client.Transport
			d.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/"+containerID+"/json") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
				}
				return prior.RoundTrip(req)
			})
			if ok, err := d.IsFenced(context.Background(), target()); ok || err == nil {
				t.Fatalf("malformed inspection accepted: %v %v", ok, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestReplacementAppearingDuringInspectionBlocksProof(t *testing.T) {
	d, f := newDaemon(t)
	f.running = false
	prior := d.client.Transport
	d.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := prior.RoundTrip(req)
		if strings.HasSuffix(req.URL.Path, "/"+containerID+"/json") {
			f.mu.Lock()
			f.ids = append(f.ids, replacementID)
			f.mu.Unlock()
		}
		return response, err
	})
	if ok, err := d.IsFenced(context.Background(), target()); ok || err == nil {
		t.Fatalf("replacement accepted: %v %v", ok, err)
	}
}

func TestAlreadyStoppedFenceIsIdempotent(t *testing.T) {
	d, f := newDaemon(t)
	f.running = false
	f.failPath = "/stop"
	f.status = 304
	if err := d.Fence(context.Background(), target()); err != nil {
		t.Fatalf("already stopped: %v", err)
	}
	if ok, err := d.IsFenced(context.Background(), target()); !ok || err != nil {
		t.Fatalf("stopped verification: %v %v", ok, err)
	}
}

func TestDockerUsesUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "maat-fencing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("unexpected socket request: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ApiVersion":"1.54","MinAPIVersion":"1.44"}`)
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	d, err := NewDocker(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.version(context.Background()); got != "/v1.54" || err != nil {
		t.Fatalf("socket version: %s %v", got, err)
	}
}
