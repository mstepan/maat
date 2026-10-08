// Package compose implements local Docker discovery and sessions behind core contracts.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maat/dashboard/internal/core"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Adapter struct {
	project string
	command func(context.Context, ...string) ([]byte, error)
	execute func(context.Context, ...string) error
	mu      sync.Mutex
	host    string
}

func New(project string) *Adapter {
	return &Adapter{project: project, command: command, execute: func(ctx context.Context, args ...string) error {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return errors.New("psql session failed or could not be launched")
		}
		return nil
	}}
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, errors.New("docker output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var output boundedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("docker CLI not found in PATH")
		}
		return nil, errors.New("docker metadata command failed or timed out; check local Engine and Docker context")
	}
	return output.Bytes(), nil
}

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var instanceID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var nodeIDs = []string{"instance-a", "instance-b", "instance-c"}

type container struct {
	ID     string `json:"Id"`
	Config struct{ Labels map[string]string }
	State  struct {
		Running, Paused bool
		Status          string
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
	}
}

func (a *Adapter) localHost(ctx context.Context) (string, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" || os.Getenv("DOCKER_CONTEXT") != "" {
		args := []string{"context", "inspect"}
		if name := os.Getenv("DOCKER_CONTEXT"); name != "" {
			args = append(args, name)
		}
		data, err := a.command(ctx, args...)
		if err != nil {
			return "", err
		}
		var contexts []struct {
			Endpoints map[string]struct{ Host string }
		}
		if json.Unmarshal(data, &contexts) != nil || len(contexts) != 1 {
			return "", errors.New("cannot resolve Docker context")
		}
		host = contexts[0].Endpoints["docker"].Host
	}
	if !strings.HasPrefix(host, "unix:///") {
		return "", errors.New("local Unix-socket Docker context required; remote Docker is unsupported")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host != "" && a.host != host {
		return "", errors.New("docker context changed; restart dashboard")
	}
	a.host = host
	return host, nil
}
func (a *Adapter) inspect(ctx context.Context, host string, ids []string) ([]container, error) {
	args := append([]string{"--host", host, "inspect", "--type", "container"}, ids...)
	data, err := a.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var records []container
	if json.Unmarshal(data, &records) != nil {
		return nil, errors.New("invalid Docker inspection response")
	}
	return records, nil
}
func (a *Adapter) valid(c container, node, cluster string) bool {
	l := c.Config.Labels
	return instanceID.MatchString(c.ID) && l["com.docker.compose.project"] == a.project && l["com.docker.compose.service"] == node && l["maat.node"] == node && l["maat.cluster"] != "" && (cluster == "" || l["maat.cluster"] == cluster)
}
func endpoint(c container) (string, error) {
	urls := map[string]bool{}
	for _, binding := range c.NetworkSettings.Ports["8000/tcp"] {
		ip := net.ParseIP(binding.HostIP)
		port, err := strconv.Atoi(binding.HostPort)
		if err != nil || port < 1 || port > 65535 || !ip.IsLoopback() {
			continue
		}
		urls["http://"+net.JoinHostPort(ip.String(), strconv.Itoa(port))] = true
	}
	if len(urls) != 1 {
		return "", errors.New("missing or ambiguous explicit loopback status port")
	}
	for u := range urls {
		return u, nil
	}
	return "", errors.New("status port missing")
}
func (a *Adapter) Discover(ctx context.Context) ([]core.Target, error) {
	if !projectName.MatchString(a.project) {
		return nil, errors.New("invalid Compose project name")
	}
	host, err := a.localHost(ctx)
	if err != nil {
		return nil, err
	}
	output, err := a.command(ctx, "--host", host, "ps", "-a", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project="+a.project, "--filter", "label=maat.node")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		return nil, fmt.Errorf("no Maat nodes found in project %s", a.project)
	}
	for _, id := range ids {
		if !instanceID.MatchString(id) {
			return nil, errors.New("invalid discovered instance ID")
		}
	}
	records, err := a.inspect(ctx, host, ids)
	if err != nil {
		return nil, err
	}
	byNode := map[string][]container{}
	cluster := ""
	for _, c := range records {
		node := c.Config.Labels["maat.node"]
		known := false
		for _, id := range nodeIDs {
			if id == node {
				known = true
			}
		}
		if !known {
			return nil, errors.New("unexpected node in selected project")
		}
		label := c.Config.Labels["maat.cluster"]
		if label == "" || (cluster != "" && label != cluster) {
			return nil, errors.New("missing or mixed Maat cluster labels")
		}
		cluster = label
		byNode[node] = append(byNode[node], c)
	}
	var targets []core.Target
	for _, node := range nodeIDs {
		t := core.Target{ID: node, ClusterID: cluster, Runtime: "missing", Error: "instance missing"}
		found := byNode[node]
		if len(found) == 1 {
			c := found[0]
			t.InstanceID = c.ID
			t.Runtime = c.State.Status
			switch {
			case !a.valid(c, node, cluster):
				t.Error = "instance label identity mismatch"
			case !c.State.Running:
				t.Runtime = "stopped"
				t.Error = "instance stopped"
			case c.State.Paused:
				t.Runtime = "paused"
				t.Error = "instance paused"
			default:
				t.Available = true
				t.Error = ""
				t.Endpoint, err = endpoint(c)
				if err != nil {
					t.Error = err.Error()
				}
			}
		} else if len(found) > 1 {
			t.Runtime = "ambiguous"
			t.Error = "multiple instances bound to node"
		}
		targets = append(targets, t)
	}
	return targets, nil
}
func (a *Adapter) Run(ctx context.Context, t core.Target) error {
	if !instanceID.MatchString(t.InstanceID) || t.ClusterID == "" {
		return errors.New("session target identity invalid")
	}
	host, err := a.localHost(ctx)
	if err != nil {
		return err
	}
	records, err := a.inspect(ctx, host, []string{t.InstanceID})
	if err != nil {
		return err
	}
	if len(records) != 1 || records[0].ID != t.InstanceID || !a.valid(records[0], t.ID, t.ClusterID) {
		return errors.New("session target identity changed")
	}
	if !records[0].State.Running || records[0].State.Paused {
		return errors.New("session requires running unpaused instance")
	}
	return a.execute(ctx, "--host", host, "exec", "-it", "--user", "postgres", t.InstanceID, "psql", "-X", "-h", "/var/lib/maat/control/postgres/socket", "-U", "postgres", "-d", "postgres")
}
