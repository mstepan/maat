package compose

import (
	"context"
	"encoding/json"
	"maat/dashboard/internal/core"
	"strings"
	"testing"
)

func instance(node string) map[string]any {
	return map[string]any{"Id": strings.Repeat(map[string]string{"instance-a": "a", "instance-b": "b", "instance-c": "c"}[node], 64), "Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": "test-lab", "com.docker.compose.service": node, "maat.node": node, "maat.cluster": "lab"}}, "State": map[string]any{"Running": true, "Paused": false, "Status": "running"}, "NetworkSettings": map[string]any{"Ports": map[string]any{"8000/tcp": []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": "29123"}}}}}
}
func adapter(t *testing.T, records []map[string]any) *Adapter {
	t.Helper()
	a := New("test-lab")
	a.command = func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "context inspect"):
			return []byte(`[{"Endpoints":{"docker":{"Host":"unix:///tmp/test.sock"}}}]`), nil
		case strings.Contains(joined, " ps "):
			return []byte(strings.Repeat("a", 64) + "\n"), nil
		case strings.Contains(joined, " inspect "):
			return json.Marshal(records)
		}
		t.Fatalf("unexpected command %v", args)
		return nil, nil
	}
	return a
}
func TestDiscoverDynamicPortsAndMissingNodes(t *testing.T) {
	a := adapter(t, []map[string]any{instance("instance-a")})
	targets, err := a.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0].Endpoint != "http://127.0.0.1:29123" || !targets[0].Available || targets[1].Runtime != "missing" {
		t.Fatalf("targets %+v", targets)
	}
}
func TestDiscoverRejectsUnsafeBindingsAndLabels(t *testing.T) {
	for _, name := range []string{"nonloopback", "duplicate", "mixed-cluster", "wrong-service", "stopped", "paused"} {
		t.Run(name, func(t *testing.T) {
			x := instance("instance-a")
			records := []map[string]any{x}
			switch name {
			case "nonloopback":
				x["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"8000/tcp": []any{map[string]any{"HostIp": "0.0.0.0", "HostPort": "29123"}}}
			case "duplicate":
				records = append(records, x)
			case "mixed-cluster":
				y := instance("instance-b")
				y["Config"].(map[string]any)["Labels"].(map[string]string)["maat.cluster"] = "other"
				records = append(records, y)
			case "wrong-service":
				x["Config"].(map[string]any)["Labels"].(map[string]string)["com.docker.compose.service"] = "instance-b"
			case "stopped":
				x["State"].(map[string]any)["Running"] = false
			case "paused":
				x["State"].(map[string]any)["Paused"] = true
			}
			targets, err := adapter(t, records).Discover(t.Context())
			if err == nil && targets[0].Error == "" {
				t.Fatalf("unsafe discovery accepted: %+v", targets)
			}
		})
	}
}
func TestRemoteDockerHostRejected(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://example.com:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	if _, err := adapter(t, []map[string]any{instance("instance-a")}).Discover(t.Context()); err == nil {
		t.Fatal("remote context allowed")
	}
}
func TestSessionRefusesStoppedOrReplacedTarget(t *testing.T) {
	x := instance("instance-a")
	a := adapter(t, []map[string]any{x})
	targets, _ := a.Discover(t.Context())
	if len(targets) == 0 {
		t.Fatal("missing discovery")
	}
	x["State"].(map[string]any)["Running"] = false
	if err := a.Run(t.Context(), targets[0]); err == nil {
		t.Fatal("stopped instance allowed")
	}
	x["State"].(map[string]any)["Running"] = true
	bad := core.Target{ID: "instance-a", ClusterID: "lab", InstanceID: strings.Repeat("f", 64)}
	if err := a.Run(t.Context(), bad); err == nil {
		t.Fatal("replacement allowed")
	}
}

func TestSessionUsesExactImmutableTargetAndNativeArguments(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	x := instance("instance-b")
	a := adapter(t, []map[string]any{x})
	targets, err := a.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	a.execute = func(_ context.Context, args ...string) error { got = args; return nil }
	if err = a.Run(t.Context(), targets[1]); err != nil {
		t.Fatal(err)
	}
	want := []string{"--host", "unix:///tmp/test.sock", "exec", "-it", "--user", "postgres", strings.Repeat("b", 64), "psql", "-X", "-h", "/var/lib/maat/control/postgres/socket", "-U", "postgres", "-d", "postgres"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("session args: %q", got)
	}
}

func TestMissingDockerCLIHasActionableError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := command(t.Context(), "context", "inspect")
	if err == nil || !strings.Contains(err.Error(), "CLI not found") {
		t.Fatalf("missing prerequisite: %v", err)
	}
}

func TestSessionRefusesTargetThatVanishesBeforeHandoff(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/test.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	a := adapter(t, []map[string]any{instance("instance-b")})
	targets, err := a.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.execute = func(context.Context, ...string) error { calls++; return nil }
	a.command = func(context.Context, ...string) ([]byte, error) { return []byte(`[]`), nil }
	if err = a.Run(t.Context(), targets[1]); err == nil || calls != 0 {
		t.Fatal("vanished target executed or fell back")
	}
}
