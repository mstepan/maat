// Package postgres controls only PostgreSQL's native processes and replication.
// Callers must validate committed cluster authority before Start or Promote, and
// again after long-running preparation; this package never grants that authority.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type Config struct {
	DataDir, StateDir, PasswordFile, ReplicationPasswordFile, NodeID string
	Port                                                             uint16
	Peers                                                            []string
}
type Upstream struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	NodeID   string `json:"node_id"`
	SystemID string `json:"system_id"`
}
type Observation struct {
	Healthy           bool   `json:"healthy"`
	Recovery          bool   `json:"recovery"`
	SystemID          string `json:"system_id"`
	Timeline          uint64 `json:"timeline"`
	FlushLSN          uint64 `json:"flush_lsn"`
	ReplayLSN         uint64 `json:"replay_lsn"`
	ReceiverStreaming bool   `json:"receiver_streaming"`
	ReceiverHost      string `json:"receiver_host"`
	ReceivedTimeline  uint64 `json:"received_timeline"`
	ReplayPaused      bool   `json:"replay_paused"`
	History           string `json:"history"`
}
type Controller struct {
	cfg         Config
	mu          sync.Mutex
	connections chan struct{}
}

func New(cfg Config) (*Controller, error) {
	if !identifier(cfg.NodeID) || cfg.Port == 0 {
		return nil, errors.New("invalid PostgreSQL node ID or port")
	}
	seen := map[string]bool{}
	slots := map[string]bool{}
	for _, p := range cfg.Peers {
		if !identifier(p) || seen[p] || slots[slot(p)] {
			return nil, errors.New("invalid or duplicate PostgreSQL peer")
		}
		seen[p] = true
		slots[slot(p)] = true
	}
	if !seen[cfg.NodeID] {
		return nil, errors.New("PostgreSQL peers omit local node")
	}
	for _, p := range []string{cfg.DataDir, cfg.StateDir} {
		if strings.ContainsAny(p, "'\\\r\n\x00") {
			return nil, errors.New("unsupported characters in managed path")
		}
		if err := checkPath(p); err != nil {
			return nil, err
		}
	}
	if cfg.DataDir == cfg.StateDir || strings.HasPrefix(cfg.DataDir, cfg.StateDir+"/") || strings.HasPrefix(cfg.StateDir, cfg.DataDir+"/") {
		return nil, errors.New("data and agent state directories overlap")
	}
	if err := ownedDir(filepath.Dir(cfg.DataDir)); err != nil {
		return nil, fmt.Errorf("managed data parent: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	if err := ownedDir(cfg.StateDir); err != nil {
		return nil, err
	}
	for _, p := range []string{cfg.PasswordFile, cfg.ReplicationPasswordFile} {
		if !filepath.IsAbs(p) || strings.HasPrefix(p, cfg.DataDir+"/") {
			return nil, errors.New("password file must be outside data directory")
		}
		if _, err := secret(p); err != nil {
			return nil, err
		}
	}
	c := &Controller{cfg: cfg, connections: make(chan struct{}, 8)}
	if len(c.socketDir()) > 90 {
		return nil, errors.New("Unix socket directory path is too long")
	}
	if err := os.MkdirAll(c.socketDir(), 0700); err != nil {
		return nil, err
	}
	if err := checkPath(c.socketDir()); err != nil {
		return nil, err
	}
	if err := ownedDir(c.socketDir()); err != nil {
		return nil, err
	}
	if err := os.Chmod(c.socketDir(), 0700); err != nil {
		return nil, err
	}
	j, err := c.readJournal()
	if err != nil {
		return nil, err
	}
	if j.Phase == "rewind_in_progress" || j.Phase == "initializing_primary" || j.Phase == "backup_in_progress" {
		j.Phase = "reinitialization_required"
		if err = c.writeJournal(j); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (c *Controller) socketDir() string { return filepath.Join(c.cfg.StateDir, "socket") }
func ParseLSN(s string) (uint64, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, errors.New("invalid PostgreSQL LSN")
	}
	var v [2]uint64
	for i, p := range parts {
		if len(p) == 0 || len(p) > 8 {
			return 0, errors.New("invalid PostgreSQL LSN")
		}
		for _, r := range p {
			if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F' || r >= 'a' && r <= 'f') {
				return 0, errors.New("invalid PostgreSQL LSN")
			}
		}
		n, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return 0, errors.New("invalid PostgreSQL LSN")
		}
		v[i] = n
	}
	return v[0]<<32 | v[1], nil
}
func (c *Controller) validUpstream(u Upstream) error {
	if u.Port == 0 || u.Host == "" || u.NodeID == c.cfg.NodeID || u.SystemID == "" {
		return errors.New("invalid upstream identity")
	}
	for _, r := range u.Host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':' || r == '_') {
			return errors.New("invalid upstream host")
		}
	}
	for _, r := range u.SystemID {
		if r < '0' || r > '9' {
			return errors.New("invalid upstream system identifier")
		}
	}
	for _, p := range c.cfg.Peers {
		if p == u.NodeID {
			return nil
		}
	}
	return errors.New("upstream is not a configured peer")
}

type connection struct {
	*pgx.Conn
	permit chan struct{}
}

func (c *Controller) connect(ctx context.Context, u *Upstream) (*connection, error) {
	conf, err := pgx.ParseConfig("host=/tmp user=postgres dbname=postgres sslmode=disable")
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	conf.Host = c.socketDir()
	conf.Port = c.cfg.Port
	conf.ConnectTimeout = 3 * time.Second
	conf.RuntimeParams = map[string]string{"application_name": "maat_agent", "statement_timeout": "3000"}
	if u != nil {
		if err = c.validUpstream(*u); err != nil {
			return nil, err
		}
		conf.Host = u.Host
		conf.Port = u.Port
		conf.Password, err = secret(c.cfg.PasswordFile)
		if err != nil {
			return nil, err
		}
	}
	select {
	case c.connections <- struct{}{}:
	case <-ctx.Done():
		return nil, errors.New("PostgreSQL connection capacity deadline exceeded")
	}
	conn, err := pgx.ConnectConfig(ctx, conf)
	if err != nil {
		<-c.connections
		return nil, errors.New("PostgreSQL connection failed")
	}
	return &connection{Conn: conn, permit: c.connections}, nil
}
func closeConn(conn *connection) {
	defer func() { <-conn.permit }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
func (c *Controller) Observe(ctx context.Context) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := c.connect(ctx, nil)
	if err != nil {
		return Observation{}, err
	}
	defer closeConn(conn)
	o, err := observe(ctx, conn)
	if err != nil {
		return o, err
	}
	if o.Timeline > 1 {
		p := filepath.Join(c.cfg.DataDir, "pg_wal", fmt.Sprintf("%08X.history", o.Timeline))
		st, e := os.Lstat(p)
		if e != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
			return Observation{}, errors.New("timeline history unavailable")
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return Observation{}, errors.New("cannot read timeline history")
		}
		o.History = string(b)
	}
	return o, nil
}
func observe(ctx context.Context, conn *connection) (Observation, error) {
	var o Observation
	var checkpoint uint64
	var replay, flush string
	err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery(), (pg_control_system()).system_identifier::text, (pg_control_checkpoint()).timeline_id::bigint`).Scan(&o.Recovery, &o.SystemID, &checkpoint)
	if err != nil {
		return o, errors.New("PostgreSQL identity observation failed")
	}
	o.Timeline = checkpoint
	if o.Recovery {
		err = conn.QueryRow(ctx, `SELECT COALESCE(pg_last_wal_replay_lsn()::text,''), pg_is_wal_replay_paused()`).Scan(&replay, &o.ReplayPaused)
		if err != nil {
			return o, errors.New("PostgreSQL replay observation failed")
		}
		o.ReplayLSN, err = ParseLSN(replay)
		if err != nil {
			return o, err
		}
		var status string
		err = conn.QueryRow(ctx, `SELECT status, sender_host, received_tli::bigint FROM pg_stat_wal_receiver`).Scan(&status, &o.ReceiverHost, &o.ReceivedTimeline)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return o, errors.New("WAL receiver observation failed")
		}
		o.ReceiverStreaming = status == "streaming"
		// received_tli can be ahead of replay. Report the confirmed restartpoint
		// timeline; eligibility may wait for a restartpoint after a timeline switch.
	} else {
		var walfile string
		err = conn.QueryRow(ctx, `SELECT pg_current_wal_flush_lsn()::text, pg_walfile_name(pg_current_wal_lsn())`).Scan(&flush, &walfile)
		if err != nil || len(walfile) != 24 {
			return o, errors.New("PostgreSQL WAL observation failed")
		}
		o.FlushLSN, err = ParseLSN(flush)
		if err != nil {
			return o, err
		}
		o.Timeline, err = strconv.ParseUint(walfile[:8], 16, 32)
		if err != nil {
			return o, errors.New("invalid WAL timeline")
		}
	}
	if o.SystemID == "" || o.Timeline == 0 {
		return o, errors.New("invalid PostgreSQL identity")
	}
	o.Healthy = true
	return o, nil
}
func (c *Controller) source(ctx context.Context, u Upstream) (Observation, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := c.connect(ctx, &u)
	if err != nil {
		return Observation{}, 0, err
	}
	defer closeConn(conn)
	o, err := observe(ctx, conn)
	if err != nil {
		return o, 0, err
	}
	if o.Recovery || o.SystemID != u.SystemID {
		return o, 0, errors.New("upstream is not the expected writable primary")
	}
	var full bool
	var size uint64
	var tablespaces int
	if err = conn.QueryRow(ctx, `SELECT current_setting('full_page_writes')::boolean, COALESCE((SELECT sum(pg_database_size(oid)) FROM pg_database),0)::bigint, (SELECT count(*) FROM pg_tablespace WHERE spcname NOT IN ('pg_default','pg_global'))`).Scan(&full, &size, &tablespaces); err != nil || !full || tablespaces != 0 {
		return o, 0, errors.New("upstream prerequisites unavailable or unsupported tablespaces")
	}
	return o, size, nil
}
func (c *Controller) VerifyReplica(ctx context.Context, u Upstream) error {
	src, _, err := c.source(ctx, u)
	if err != nil {
		return err
	}
	o, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if !o.Recovery || o.SystemID != src.SystemID || !o.ReceiverStreaming || o.ReceiverHost != u.Host || o.ReplayPaused || o.ReplayLSN < src.FlushLSN {
		return errors.New("replica has not verified streaming replay through upstream watermark")
	}
	// sender_host is the configured host (not reverse DNS); verify actual config
	// as well, since addresses may be reported differently by PostgreSQL.
	conn, err := c.connect(ctx, nil)
	if err != nil {
		return err
	}
	defer closeConn(conn)
	var configured, slotName string
	if err = conn.QueryRow(ctx, `SELECT current_setting('primary_conninfo'), current_setting('primary_slot_name')`).Scan(&configured, &slotName); err != nil {
		return errors.New("cannot verify replica upstream")
	}
	expected, err := c.connectionString(u, "maat_repl")
	if err != nil {
		return err
	}
	if configured != expected || slotName != slot(c.cfg.NodeID) {
		return errors.New("replica follows an unexpected upstream")
	}
	if o.ReceivedTimeline != src.Timeline {
		return errors.New("WAL receiver timeline does not match upstream")
	}
	return nil
}
func run(ctx context.Context, name string, args ...string) error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to execute PostgreSQL tools as root")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	// Tool output can contain connection strings or credential context. Return
	// only the tool name and exit status, never stdout/stderr or arguments.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s interrupted: %w", name, ctx.Err())
		}
		var x *exec.ExitError
		if errors.As(err, &x) {
			return fmt.Errorf("%s failed (exit %d)", name, x.ExitCode())
		}
		return fmt.Errorf("%s could not execute", name)
	}
	return nil
}
func (c *Controller) running(ctx context.Context) (bool, error) {
	if !c.Exists() {
		empty, err := c.Empty()
		if err != nil {
			return false, err
		}
		if empty {
			return false, nil
		}
		return false, errors.New("cannot establish stopped state of unrecognized data")
	}
	if os.Geteuid() == 0 {
		return false, errors.New("refusing to execute PostgreSQL tools as root")
	}
	cmd := exec.CommandContext(ctx, "pg_ctl", "-D", c.cfg.DataDir, "status")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var x *exec.ExitError
	if errors.As(err, &x) && x.ExitCode() == 3 {
		return false, nil
	}
	return false, errors.New("cannot verify PostgreSQL process state")
}
