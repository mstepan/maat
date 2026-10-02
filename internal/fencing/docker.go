// Package fencing provides infrastructure isolation independent of PostgreSQL reachability.
package fencing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Target binds a node to one immutable container incarnation.
type Target struct {
	ClusterID   string `json:"cluster_id"`
	NodeID      string `json:"node_id"`
	ContainerID string `json:"container_id"`
}

// Fencer separates requesting isolation from observing its result.
type Fencer interface {
	Fence(context.Context, Target) error
	IsFenced(context.Context, Target) (bool, error)
}

// Docker trusts the local daemon and the managed PostgreSQL startup path.
// An administrator restarting a container invalidates earlier fencing evidence.
type Docker struct {
	client  *http.Client
	base    string
	timeout time.Duration
}

func NewDocker(socket string, timeout time.Duration) (*Docker, error) {
	if !filepath.IsAbs(socket) || strings.ContainsRune(socket, 0) || timeout <= 0 {
		return nil, errors.New("Docker fencing requires an absolute socket path and positive timeout")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Docker{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "http://docker", timeout: timeout}, nil
}

type container struct {
	ID         string `json:"Id"`
	Config     *struct{ Labels map[string]string }
	HostConfig *struct{ RestartPolicy *struct{ Name *string } }
	State      *struct{ Running, Restarting, Paused *bool }
}

// Resolve obtains identity only; even a stopped container can be resolved.
// This does not grant permission to start PostgreSQL or to promote it.
func (d *Docker) Resolve(ctx context.Context, clusterID, nodeID string) (Target, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	target := Target{ClusterID: clusterID, NodeID: nodeID}
	if clusterID == "" || nodeID == "" {
		return Target{}, errors.New("cluster and node identity are required")
	}
	version, err := d.version(ctx)
	if err != nil {
		return Target{}, err
	}
	id, err := d.lookup(ctx, version, target)
	if err != nil {
		return Target{}, err
	}
	target.ContainerID = id
	c, err := d.inspect(ctx, version, target)
	if err != nil {
		return Target{}, err
	}
	if *c.State.Paused || *c.State.Restarting {
		return Target{}, errors.New("container is paused or restarting")
	}
	return target, nil
}

// Fence requests a bounded stop. Its success alone is not proof of isolation.
func (d *Docker) Fence(ctx context.Context, target Target) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	version, _, err := d.checked(ctx, target)
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	seconds := max(0, int(time.Until(deadline)/time.Second))
	return d.request(ctx, http.MethodPost, version+"/containers/"+target.ContainerID+"/stop?t="+strconv.Itoa(seconds), nil)
}

// IsFenced always obtains fresh identity and state; a 404 is uncertainty.
func (d *Docker) IsFenced(ctx context.Context, target Target) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	_, c, err := d.checked(ctx, target)
	if err != nil {
		return false, err
	}
	return !*c.State.Running && !*c.State.Restarting && !*c.State.Paused, nil
}

func (d *Docker) checked(ctx context.Context, target Target) (string, *container, error) {
	if target.ClusterID == "" || target.NodeID == "" || !fullID(target.ContainerID) {
		return "", nil, errors.New("invalid fencing target identity")
	}
	version, err := d.version(ctx)
	if err != nil {
		return "", nil, err
	}
	id, err := d.lookup(ctx, version, target)
	if err != nil {
		return "", nil, err
	}
	if id != target.ContainerID {
		return "", nil, errors.New("container incarnation changed")
	}
	c, err := d.inspect(ctx, version, target)
	if err != nil {
		return "", nil, err
	}
	// Recheck after inspection so a concurrently visible replacement blocks use
	// of the inspected state. Docker offers no atomic stop-and-label transaction.
	id, err = d.lookup(ctx, version, target)
	if err != nil {
		return "", nil, err
	}
	if id != target.ContainerID {
		return "", nil, errors.New("container incarnation changed")
	}
	return version, c, nil
}

func (d *Docker) lookup(ctx context.Context, version string, target Target) (string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"maat.cluster=" + target.ClusterID, "maat.node=" + target.NodeID}})
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}
	var rows []struct {
		ID     string `json:"Id"`
		Labels map[string]string
	}
	if err := d.request(ctx, http.MethodGet, version+"/containers/json?"+query.Encode(), &rows); err != nil {
		return "", err
	}
	if len(rows) != 1 || !fullID(rows[0].ID) || !labelsMatch(rows[0].Labels, target) {
		return "", errors.New("container label identity is missing, ambiguous, or invalid")
	}
	return rows[0].ID, nil
}

func (d *Docker) inspect(ctx context.Context, version string, target Target) (*container, error) {
	var c container
	if err := d.request(ctx, http.MethodGet, version+"/containers/"+target.ContainerID+"/json", &c); err != nil {
		return nil, err
	}
	if c.ID != target.ContainerID || c.Config == nil || !labelsMatch(c.Config.Labels, target) {
		return nil, errors.New("container inspection identity mismatch")
	}
	if c.HostConfig == nil || c.HostConfig.RestartPolicy == nil || c.HostConfig.RestartPolicy.Name == nil {
		return nil, errors.New("container restart policy is unknown")
	}
	policy := *c.HostConfig.RestartPolicy.Name
	if policy != "no" && policy != "" {
		return nil, errors.New("container automatic restart must be disabled")
	}
	if c.State == nil || c.State.Running == nil || c.State.Restarting == nil || c.State.Paused == nil {
		return nil, errors.New("container state is incomplete")
	}
	return &c, nil
}

func labelsMatch(labels map[string]string, target Target) bool {
	return labels["maat.cluster"] == target.ClusterID && labels["maat.node"] == target.NodeID
}

func fullID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (d *Docker) version(ctx context.Context) (string, error) {
	var v struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string
	}
	if err := d.request(ctx, http.MethodGet, "/version", &v); err != nil {
		return "", err
	}
	high, err := apiMinor(v.APIVersion)
	if err != nil {
		return "", err
	}
	low, err := apiMinor(v.MinAPIVersion)
	if err != nil {
		return "", err
	}
	if low > high || high < 44 || low > 54 {
		return "", errors.New("Docker API has no supported version overlap (1.44 through 1.54)")
	}
	return "/v1." + strconv.Itoa(min(high, 54)), nil
}

func apiMinor(version string) (int, error) {
	major, minor, ok := strings.Cut(version, ".")
	n, err := strconv.Atoi(minor)
	if !ok || major != "1" || err != nil || n < 0 || strconv.Itoa(n) != minor {
		return 0, errors.New("invalid Docker API version")
	}
	return n, nil
}

func (d *Docker) request(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, nil)
	if err != nil {
		return errors.New("invalid Docker request")
	}
	response, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Docker request interrupted: %w", ctx.Err())
		}
		return errors.New("Docker daemon request failed")
	}
	defer response.Body.Close()
	if method == http.MethodPost && (response.StatusCode == 204 || response.StatusCode == 304) {
		return nil
	}
	if response.StatusCode != http.StatusOK || out == nil {
		return fmt.Errorf("Docker API returned HTTP %d", response.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return errors.New("Docker response could not be read")
	}
	if len(body) > maxBody {
		return errors.New("Docker response exceeds size limit")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("invalid Docker JSON response")
	}
	return nil
}
