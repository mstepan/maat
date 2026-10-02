package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Node struct {
	ID              string `json:"id"`
	RaftAddress     string `json:"raft_address"`
	HTTPAddress     string `json:"http_address"`
	PostgresAddress string `json:"postgres_address"`
}
type Config struct {
	ID                         string `json:"id"`
	ClusterID                  string `json:"cluster_id"`
	BootstrapSeed              string `json:"bootstrap_seed"`
	InitialPrimary             string `json:"initial_primary"`
	DataDir                    string `json:"data_dir"`
	StateDir                   string `json:"state_dir"`
	PasswordFile               string `json:"password_file"`
	ReplicationPasswordFile    string `json:"replication_password_file"`
	DockerSocket               string `json:"docker_socket"`
	Nodes                      []Node `json:"nodes"`
	MaxPromotionLagBytes       uint64 `json:"max_promotion_lag_bytes"`
	MaxObservationAgeSeconds   int    `json:"max_observation_age_seconds"`
	ObservationIntervalSeconds int    `json:"observation_interval_seconds"`
	FailureThreshold           int    `json:"failure_threshold"`
	OperationTimeoutSeconds    int    `json:"operation_timeout_seconds"`
	RecoveryTimeoutSeconds     int    `json:"recovery_timeout_seconds"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var nodeIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func ReadConfig(r io.Reader) (Config, error) {
	c := Config{MaxPromotionLagBytes: 16 * 1024 * 1024, MaxObservationAgeSeconds: 30, ObservationIntervalSeconds: 1, FailureThreshold: 3, OperationTimeoutSeconds: 10, RecoveryTimeoutSeconds: 120}
	d := json.NewDecoder(io.LimitReader(r, 1024*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	if !identifier.MatchString(c.ClusterID) || len(c.Nodes) != 3 {
		return c, fmt.Errorf("cluster ID must be a lowercase identifier and exactly three nodes are required")
	}
	ids, addresses := map[string]bool{}, map[string]bool{}
	for _, n := range c.Nodes {
		if !nodeIdentifier.MatchString(n.ID) || ids[n.ID] {
			return c, fmt.Errorf("invalid or duplicate node ID")
		}
		ids[n.ID] = true
		for _, a := range []string{n.RaftAddress, n.HTTPAddress, n.PostgresAddress} {
			host, port, e := net.SplitHostPort(a)
			p, pe := strconv.Atoi(port)
			if e != nil || pe != nil || host == "" || p < 1 || p > 65535 || addresses[a] || strings.ContainsAny(host, " /\\\t\n\r") {
				return c, fmt.Errorf("invalid or duplicate node address: %q", a)
			}
			addresses[a] = true
		}
	}
	if !ids[c.ID] || !ids[c.BootstrapSeed] || !ids[c.InitialPrimary] {
		return c, fmt.Errorf("local, bootstrap and initial-primary IDs must be members")
	}
	for _, p := range []string{c.DataDir, c.StateDir, c.PasswordFile, c.ReplicationPasswordFile, c.DockerSocket} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return c, fmt.Errorf("paths must be absolute, clean, non-root paths")
		}
	}
	if c.DataDir == c.StateDir || strings.HasPrefix(c.DataDir, c.StateDir+"/") || strings.HasPrefix(c.StateDir, c.DataDir+"/") {
		return c, fmt.Errorf("database and control paths must not overlap")
	}
	for _, p := range []string{c.PasswordFile, c.ReplicationPasswordFile, c.DockerSocket} {
		if p == c.DataDir || p == c.StateDir || strings.HasPrefix(p, c.DataDir+"/") || strings.HasPrefix(p, c.StateDir+"/") {
			return c, fmt.Errorf("secrets and Docker socket must be outside managed storage")
		}
	}
	if c.PasswordFile == c.ReplicationPasswordFile {
		return c, fmt.Errorf("control and replication credentials require separate files")
	}
	if c.ObservationIntervalSeconds < 1 || c.ObservationIntervalSeconds > 60 || c.MaxObservationAgeSeconds < c.ObservationIntervalSeconds || c.MaxObservationAgeSeconds > 3600 || c.FailureThreshold < 1 || c.FailureThreshold > 100 || c.OperationTimeoutSeconds < 1 || c.OperationTimeoutSeconds > 120 || c.RecoveryTimeoutSeconds < c.OperationTimeoutSeconds || c.RecoveryTimeoutSeconds > 3600 {
		return c, fmt.Errorf("invalid observation, failure or timeout limits")
	}
	return c, nil
}
func (c Config) Node(id string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}
func (c Config) Timeout() time.Duration {
	return time.Duration(c.OperationTimeoutSeconds) * time.Second
}
