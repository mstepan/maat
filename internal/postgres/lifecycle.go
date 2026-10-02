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
	"syscall"
	"time"
)

func (c *Controller) control(ctx context.Context, path string) (map[string]string, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("refusing to execute PostgreSQL tools as root")
	}
	cmd := exec.CommandContext(ctx, "pg_controldata", path)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil, errors.New("cannot inspect PostgreSQL control file")
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if m["Database system identifier"] == "" {
		return nil, errors.New("invalid PostgreSQL control file")
	}
	return m, nil
}
func (c *Controller) requireStopped(ctx context.Context) error {
	running, err := c.running(ctx)
	if err != nil {
		return err
	}
	if running {
		return errors.New("PostgreSQL must be verified stopped")
	}
	return nil
}
func (c *Controller) InitializePrimary(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	empty, err := c.Empty()
	if err != nil {
		return err
	}
	if !empty {
		return errors.New("primary initialization requires an empty managed directory")
	}
	if err = c.writeJournal(journal{Phase: "initializing_primary"}); err != nil {
		return err
	}
	if err = run(ctx, "initdb", "-D", c.cfg.DataDir, "--username=postgres", "--pwfile="+c.cfg.PasswordFile, "--data-checksums", "--auth-local=trust", "--auth-host=scram-sha-256", "--encoding=UTF8", "--locale=C"); err != nil {
		return c.recoveryFailure(err)
	}
	if err = c.configure(c.cfg.DataDir, nil); err != nil {
		return c.recoveryFailure(err)
	}
	if err = c.validateData(c.cfg.DataDir); err != nil {
		return c.recoveryFailure(err)
	}
	ctl, err := c.control(ctx, c.cfg.DataDir)
	if err != nil {
		return c.recoveryFailure(err)
	}
	return c.writeJournal(journal{Phase: "primary_ready", SystemID: ctl["Database system identifier"]})
}
func (c *Controller) recoveryFailure(cause error) error {
	j, err := c.readJournal()
	if err != nil {
		return fmt.Errorf("recovery failed; journal unavailable: %w", err)
	}
	j.Phase = "reinitialization_required"
	if err = c.writeJournal(j); err != nil {
		return fmt.Errorf("recovery failed; cannot persist recovery exclusion: %w", err)
	}
	return fmt.Errorf("reinitialization_required: %w", cause)
}
func availableSpace(path string, size uint64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return errors.New("cannot inspect recovery free space")
	}
	// Allow database bytes plus 1 GiB for WAL and backup overhead. Retained files
	// are already allocated and therefore already excluded from available bytes.
	available := uint64(st.Bavail) * uint64(st.Bsize)
	if size > ^uint64(0)-(1<<30) || available < size+(1<<30) {
		return errors.New("insufficient free space for retained data and fresh backup")
	}
	return nil
}
func (c *Controller) backup(ctx context.Context, u Upstream, path string) error {
	conn, err := c.connectionString(u, "maat_repl")
	if err != nil {
		return err
	}
	if err = run(ctx, "pg_basebackup", "--dbname="+conn, "--pgdata="+path, "--wal-method=stream", "--checkpoint=fast", "--write-recovery-conf", "--slot="+slot(c.cfg.NodeID), "--no-password"); err != nil {
		return err
	}
	if err = c.validateData(path); err != nil {
		return err
	}
	// Native manifest verification catches incomplete/interrupted backups.
	if err = run(ctx, "pg_verifybackup", path); err != nil {
		return err
	}
	ctl, err := c.control(ctx, path)
	if err != nil {
		return err
	}
	if ctl["Database system identifier"] != u.SystemID {
		return errors.New("backup database identity changed")
	}
	if _, _, err = c.source(ctx, u); err != nil {
		return err
	}
	return c.configure(path, &u)
}
func (c *Controller) InitializeReplica(ctx context.Context, u Upstream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := c.validUpstream(u); err != nil {
		return err
	}
	empty, err := c.Empty()
	if err != nil {
		return err
	}
	if !empty {
		return errors.New("replica initialization requires an empty managed directory")
	}
	_, size, err := c.source(ctx, u)
	if err != nil {
		return err
	}
	if err = availableSpace(filepath.Dir(c.cfg.DataDir), size); err != nil {
		return err
	}
	if err = c.writeJournal(journal{Phase: "backup_in_progress", Upstream: u, SystemID: u.SystemID}); err != nil {
		return err
	}
	if err = c.backup(ctx, u, c.cfg.DataDir); err != nil {
		return c.recoveryFailure(err)
	}
	return c.writeJournal(journal{Phase: "standby_ready", Upstream: u, SystemID: u.SystemID})
}
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := c.startAllowed(); err != nil {
		return err
	}
	if err := c.validateData(c.cfg.DataDir); err != nil {
		return err
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	ctl, err := c.control(ctx, c.cfg.DataDir)
	if err != nil {
		return err
	}
	if ctl["Database system identifier"] != j.SystemID {
		return errors.New("managed database identity differs from preparation journal")
	}
	running, err := c.running(ctx)
	if err != nil {
		return err
	}
	if !running {
		if j.Phase == "standby_ready" {
			st, e := os.Lstat(filepath.Join(c.cfg.DataDir, "standby.signal"))
			if e != nil || !st.Mode().IsRegular() {
				return errors.New("prepared replica lacks standby signal")
			}
		}
		if err = run(ctx, "pg_ctl", "-D", c.cfg.DataDir, "-l", filepath.Join(c.cfg.StateDir, "postgres.log"), "-w", "-t", "30", "start"); err != nil {
			return err
		}
	}
	o, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if j.Phase == "standby_ready" && !o.Recovery {
		return errors.New("prepared standby unexpectedly writable; caller must stop unauthorized primary")
	}
	if j.Phase == "primary_ready" && o.Recovery {
		return errors.New("prepared primary remains in recovery")
	}
	if !o.Recovery {
		return c.ensurePrimary(ctx)
	}
	return nil
}
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	running, err := c.running(ctx)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	commandErr := run(ctx, "pg_ctl", "-D", c.cfg.DataDir, "-m", "fast", "-w", "-t", "30", "stop")
	running, err = c.running(ctx)
	if err != nil {
		return err
	}
	if running {
		if commandErr != nil {
			return commandErr
		}
		return errors.New("PostgreSQL remained running after stop")
	}
	return nil
}
func (c *Controller) EnsurePrimary(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ensurePrimary(ctx)
}
func (c *Controller) ensurePrimary(ctx context.Context) error {
	conn, err := c.connect(ctx, nil)
	if err != nil {
		return err
	}
	defer closeConn(conn)
	o, err := observe(ctx, conn)
	if err != nil {
		return err
	}
	if o.Recovery {
		return errors.New("cannot provision replication roles on a standby")
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	if j.SystemID != o.SystemID || (j.Phase != "primary_ready" && j.Phase != "standby_ready") {
		return errors.New("writable database lacks matching completed preparation")
	}
	var exists bool
	if err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='maat_repl')").Scan(&exists); err != nil {
		return errors.New("cannot inspect replication role")
	}
	password, err := secret(c.cfg.ReplicationPasswordFile)
	if err != nil {
		return err
	}
	// Password utility statements cannot use bind placeholders. Quote the value
	// under standard_conforming_strings and suppress statement/error SQL logging.
	if _, err = conn.Exec(ctx, "SET standard_conforming_strings=on; SET log_statement='none'; SET log_min_error_statement='panic'"); err != nil {
		return errors.New("cannot secure credential provisioning session")
	}
	if !exists {
		if _, err = conn.Exec(ctx, "CREATE ROLE maat_repl LOGIN REPLICATION PASSWORD "+sqlQuote(password)); err != nil {
			return errors.New("replication role creation failed")
		}
	}
	for _, p := range c.cfg.Peers {
		if p == c.cfg.NodeID {
			continue
		}
		var present bool
		if err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name=$1 AND slot_type='physical')", slot(p)).Scan(&present); err != nil {
			return errors.New("cannot inspect replication slots")
		}
		if !present {
			if _, err = conn.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)", slot(p)); err != nil {
				return errors.New("cannot create physical replication slot")
			}
		}
	}

	if j.Phase != "primary_ready" {
		j.Phase = "primary_ready"
		j.Upstream = Upstream{}
		if err = c.writeJournal(j); err != nil {
			return err
		}
	}
	return nil
}
func (c *Controller) Promote(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if err := c.startAllowed(); err != nil {
		return err
	}
	o, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if o.Recovery {
		if o.ReplayPaused {
			return errors.New("cannot promote with paused replay")
		}
		conn, err := c.connect(ctx, nil)
		if err != nil {
			return err
		}
		// pg_promote's own wait is bounded; statement_timeout must cover that wait.
		if _, err = conn.Exec(ctx, "SET statement_timeout='35000'"); err != nil {
			closeConn(conn)
			return errors.New("cannot configure promotion timeout")
		}
		var done bool
		err = conn.QueryRow(ctx, "SELECT pg_promote(true,30)").Scan(&done)
		closeConn(conn)
		if err != nil || !done {
			return errors.New("promotion outcome pending observation")
		}
	}
	o, err = c.Observe(ctx)
	if err != nil {
		return err
	}
	if o.Recovery {
		return errors.New("promotion has not reached writable role")
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	j.Phase = "primary_ready"
	j.SystemID = o.SystemID
	j.Upstream = Upstream{}
	if err = c.writeJournal(j); err != nil {
		return err
	}
	return c.ensurePrimary(ctx)
}
func (c *Controller) Follow(ctx context.Context, u Upstream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, _, err := c.source(ctx, u); err != nil {
		return err
	}
	running, err := c.running(ctx)
	if err != nil {
		return err
	}
	if !running {
		if err = c.startAllowed(); err != nil {
			return err
		}
		if err = c.validateData(c.cfg.DataDir); err != nil {
			return err
		}
		st, e := os.Lstat(filepath.Join(c.cfg.DataDir, "standby.signal"))
		if e != nil || !st.Mode().IsRegular() {
			return errors.New("stopped writable data requires rewind before following")
		}
		ctl, e := c.control(ctx, c.cfg.DataDir)
		if e != nil {
			return e
		}
		if ctl["Database system identifier"] != u.SystemID {
			return errors.New("stopped standby identity differs from upstream")
		}
		if err = c.configure(c.cfg.DataDir, &u); err != nil {
			return err
		}
		j, e := c.readJournal()
		if e != nil {
			return e
		}
		j.Phase = "standby_ready"
		j.Upstream = u
		j.SystemID = u.SystemID
		return c.writeJournal(j)
	}

	o, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if !o.Recovery || o.SystemID != u.SystemID {
		return errors.New("only a compatible running standby may change upstream")
	}
	// Skip writes/reload on unchanged configuration; still verify current replay.
	conn, err := c.connect(ctx, nil)
	if err != nil {
		return err
	}
	defer closeConn(conn)
	var current, slotName string
	if err = conn.QueryRow(ctx, "SELECT current_setting('primary_conninfo'), current_setting('primary_slot_name')").Scan(&current, &slotName); err != nil {
		return errors.New("cannot observe standby configuration")
	}
	desired, err := c.connectionString(u, "maat_repl")
	if err != nil {
		return err
	}
	if current != desired || slotName != slot(c.cfg.NodeID) {
		if err = c.configure(c.cfg.DataDir, &u); err != nil {
			return err
		}
		var ok bool
		if err = conn.QueryRow(ctx, "SELECT pg_reload_conf()").Scan(&ok); err != nil || !ok {
			return errors.New("standby reload has not been verified")
		}
		j, err := c.readJournal()
		if err != nil {
			return err
		}
		j.Phase = "standby_ready"
		j.Upstream = u
		j.SystemID = u.SystemID
		if err = c.writeJournal(j); err != nil {
			return err
		}
	}
	return c.VerifyReplica(ctx, u)
}
func (c *Controller) Rewind(ctx context.Context, u Upstream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := c.validUpstream(u); err != nil {
		return err
	}
	if err := c.requireStopped(ctx); err != nil {
		return err
	}
	if err := c.validateData(c.cfg.DataDir); err != nil {
		return err
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	if j.Phase == "reinitialization_required" || j.Phase == "rewind_in_progress" {
		return errors.New("reinitialization_required")
	}
	ctl, err := c.control(ctx, c.cfg.DataDir)
	if err != nil {
		return err
	}
	if ctl["Database system identifier"] != u.SystemID {
		return c.recoveryFailure(errors.New("rewind database identity mismatch"))
	}
	checksums, _ := strconv.Atoi(ctl["Data page checksum version"])
	if ctl["Latest checkpoint's full_page_writes"] != "on" {
		return c.recoveryFailure(errors.New("rewind target lacks full-page-write checkpoint evidence"))
	}
	if checksums == 0 && ctl["wal_log_hints setting"] != "on" {
		return c.recoveryFailure(errors.New("rewind requires checksums or WAL hints"))
	}
	if _, _, err = c.source(ctx, u); err != nil {
		return err
	}
	conn, err := c.connectionString(u, "postgres")
	if err != nil {
		return err
	}
	j.Phase = "rewind_in_progress"
	j.Upstream = u
	j.SystemID = u.SystemID
	if err = c.writeJournal(j); err != nil {
		return err
	}
	// PostgreSQL completes crash recovery in single-user mode without client
	// listeners, then validates history/WAL before rewinding. A preliminary dry
	// run cannot complete that recovery and incorrectly rejects crashed targets.
	args := []string{"--target-pgdata=" + c.cfg.DataDir, "--source-server=" + conn}
	if err = run(ctx, "pg_rewind", args...); err != nil {
		return c.recoveryFailure(err)
	}
	if err = c.configure(c.cfg.DataDir, &u); err != nil {
		return c.recoveryFailure(err)
	}
	if err = c.validateData(c.cfg.DataDir); err != nil {
		return c.recoveryFailure(err)
	}
	if _, _, err = c.source(ctx, u); err != nil {
		return c.recoveryFailure(err)
	}
	j.Phase = "standby_ready"
	return c.writeJournal(j)
}

// Reinitialize requires the caller to validate a committed, current-generation
// operator request. It retains every old copy and returns with PostgreSQL stopped.
func (c *Controller) Reinitialize(ctx context.Context, u Upstream, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if len(requestID) > 80 || !identifier(strings.ReplaceAll(requestID, "-", "_")) {
		return errors.New("invalid recovery request ID")
	}
	if err := c.validUpstream(u); err != nil {
		return err
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	if j.RequestID == requestID && j.Phase == "standby_ready" {
		if j.Upstream != u {
			return errors.New("completed rebuild has a different upstream")
		}
		return c.validateData(c.cfg.DataDir)
	}
	if err = c.requireStopped(ctx); err != nil {
		return err
	}
	_, size, err := c.source(ctx, u)
	if err != nil {
		return err
	}
	parent := filepath.Dir(c.cfg.DataDir)
	if err = ownedDir(parent); err != nil {
		return err
	}
	if err = availableSpace(parent, size); err != nil {
		return err
	}
	retained := c.cfg.DataDir + ".retained-" + requestID
	staging := c.cfg.DataDir + ".new-" + requestID
	if j.RequestID == requestID {
		retained = j.Retained
	} else {
		phase := "reinit_prepared"
		target := c.cfg.DataDir
		// A failed earlier backup may have already retained the original and left
		// no selected data. A new committed request may reuse that retained identity.
		if _, e := os.Lstat(target); os.IsNotExist(e) && j.Phase == "reinitialization_required" && j.Retained != "" {
			retained = j.Retained
			target = retained
			phase = "reinit_retained"
		}
		if err = c.validateData(target); err != nil {
			return err
		}
		ctl, e := c.control(ctx, target)
		if e != nil {
			return e
		}
		if ctl["Database system identifier"] != u.SystemID {
			return errors.New("rebuild target identity differs from source")
		}
		if _, e = os.Lstat(staging); !os.IsNotExist(e) {
			return errors.New("rebuild staging destination already exists or cannot be inspected")
		}
		if phase == "reinit_prepared" {
			if _, e = os.Lstat(retained); !os.IsNotExist(e) {
				return errors.New("retained destination already exists or cannot be inspected")
			}
		}
		j = journal{Phase: phase, RequestID: requestID, Upstream: u, SystemID: u.SystemID, Retained: retained, Staging: staging}
		if err = c.writeJournal(j); err != nil {
			return err
		}
	}
	if filepath.Dir(retained) != parent || !strings.HasPrefix(retained, c.cfg.DataDir+".retained-") || filepath.Clean(retained) != retained {
		return errors.New("unsafe retained recovery path")
	}
	if j.Upstream != u || j.Staging != staging {
		return errors.New("rebuild source or paths changed; request must be revalidated")
	}

	switch j.Phase {
	case "reinit_prepared":
		_, oldErr := os.Lstat(retained)
		_, dataErr := os.Lstat(c.cfg.DataDir)
		if os.IsNotExist(oldErr) && dataErr == nil {
			if err = c.safeData(c.cfg.DataDir); err != nil {
				return err
			}
			if err = os.Rename(c.cfg.DataDir, retained); err != nil {
				return err
			}
			if err = syncDir(parent); err != nil {
				return err
			}
		} else if oldErr != nil || !os.IsNotExist(dataErr) {
			return errors.New("ambiguous retained directory state")
		}
		j.Phase = "reinit_retained"
		if err = c.writeJournal(j); err != nil {
			return err
		}
		fallthrough
	case "reinit_retained":
		j.Phase = "reinit_backup"
		if err = c.writeJournal(j); err != nil {
			return err
		}
		if err = c.backup(ctx, u, staging); err != nil {
			return c.recoveryFailure(err)
		}
		j.Phase = "reinit_selecting"
		if err = c.writeJournal(j); err != nil {
			return err
		}
		fallthrough
	case "reinit_selecting":
		_, stageErr := os.Lstat(staging)
		_, dataErr := os.Lstat(c.cfg.DataDir)
		if stageErr == nil && os.IsNotExist(dataErr) {
			if err = c.validateData(staging); err != nil {
				return err
			}
			if err = os.Rename(staging, c.cfg.DataDir); err != nil {
				return err
			}
			if err = syncDir(parent); err != nil {
				return err
			}
		} else if !os.IsNotExist(stageErr) || dataErr != nil {
			return errors.New("ambiguous selected backup state")
		}
		if err = c.validateData(c.cfg.DataDir); err != nil {
			return err
		}
		if _, _, err = c.source(ctx, u); err != nil {
			return err
		}
		j.Phase = "standby_ready"
		return c.writeJournal(j)
	case "reinit_backup":
		// A crash may have happened after native backup completed but before its
		// phase commit. Verify the manifest before selecting any interrupted copy.
		if err = c.validateData(staging); err != nil {
			return c.recoveryFailure(err)
		}
		if err = run(ctx, "pg_verifybackup", "--ignore=postgresql.conf", "--ignore=postgresql.auto.conf", "--ignore=pg_hba.conf", "--ignore=standby.signal", staging); err != nil {
			return c.recoveryFailure(err)
		}
		ctl, e := c.control(ctx, staging)
		if e != nil {
			return c.recoveryFailure(e)
		}
		if ctl["Database system identifier"] != u.SystemID {
			return c.recoveryFailure(errors.New("interrupted backup identity mismatch"))
		}
		if err = c.configure(staging, &u); err != nil {
			return c.recoveryFailure(err)
		}
		j.Phase = "reinit_selecting"
		if err = c.writeJournal(j); err != nil {
			return err
		}
		// Leave selection to the next reconciliation after source revalidation.
		return errors.New("verified backup prepared; retry selection with current authority")
	case "reinitialization_required":
		return errors.New("reinitialization_required: incomplete backup retained; a new operator request is required")
	default:
		return errors.New("unexpected rebuild phase")
	}
}

// RecoverAuthorizedPromotion repairs only a committed promotion's local crash
// boundary. The caller must freshly verify authority, quorum, and the old
// primary's fence before calling, then revalidate authority before Start.
// Unlike Start, this can inspect a prepared standby whose signal disappeared.
func (c *Controller) RecoverAuthorizedPromotion(ctx context.Context, expectedSystemID string, oldTimeline, minimumLSN uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if expectedSystemID == "" || oldTimeline == 0 {
		return errors.New("promotion recovery requires committed identity and timeline")
	}
	if err := c.validateData(c.cfg.DataDir); err != nil {
		return err
	}
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	if j.Phase != "standby_ready" && j.Phase != "primary_ready" {
		return errors.New("promotion recovery requires completed data preparation")
	}
	ctl, err := c.control(ctx, c.cfg.DataDir)
	if err != nil {
		return err
	}
	if j.SystemID != expectedSystemID || ctl["Database system identifier"] != expectedSystemID {
		return errors.New("promotion recovery database identity mismatch")
	}
	if j.Phase == "primary_ready" {
		return nil
	}
	signal, err := os.Lstat(filepath.Join(c.cfg.DataDir, "standby.signal"))
	if err == nil {
		if !signal.Mode().IsRegular() {
			return errors.New("invalid standby signal")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err = c.requireStopped(ctx); err != nil {
		return err
	}
	if _, err = os.Lstat(filepath.Join(c.cfg.DataDir, "recovery.signal")); !os.IsNotExist(err) {
		return errors.New("promotion recovery cannot use archive recovery signal")
	}
	// Signal removal alone is not promotion evidence. In particular, never run
	// single-user recovery on an archive-recovery control state: doing so could
	// turn an unpromoted standby with a missing signal into a writer.
	state := ctl["Database cluster state"]
	switch state {
	case "shut down", "in production", "shutting down", "in crash recovery":
	default:
		return errors.New("missing standby signal without primary control-state evidence")
	}
	if state != "shut down" {
		// With no stdin, os/exec supplies EOF. Single-user mode completes crash
		// recovery and a shutdown checkpoint without opening a client listener.
		if err = run(ctx, "postgres", "--single", "-D", c.cfg.DataDir, "postgres"); err != nil {
			return err
		}
		if err = c.requireStopped(ctx); err != nil {
			return err
		}
		ctl, err = c.control(ctx, c.cfg.DataDir)
		if err != nil {
			return err
		}
	}
	timeline, err := strconv.ParseUint(ctl["Latest checkpoint's TimeLineID"], 10, 32)
	if err != nil || timeline <= oldTimeline {
		return errors.New("promotion recovery has no advanced checkpoint timeline")
	}
	checkpoint, err := ParseLSN(ctl["Latest checkpoint location"])
	if err != nil || checkpoint < minimumLSN {
		return errors.New("promotion recovery is below the committed replay watermark")
	}
	if ctl["Database cluster state"] != "shut down" || ctl["Database system identifier"] != expectedSystemID {
		return errors.New("promotion recovery did not verify clean stopped identity")
	}
	j.Phase = "primary_ready"
	j.Upstream = Upstream{}
	return c.writeJournal(j)
}
