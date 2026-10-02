package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type journal struct {
	Version   int      `json:"version"`
	Phase     string   `json:"phase"`
	RequestID string   `json:"request_id,omitempty"`
	Upstream  Upstream `json:"upstream,omitempty"`
	SystemID  string   `json:"system_id,omitempty"`
	Retained  string   `json:"retained,omitempty"`
	Staging   string   `json:"staging,omitempty"`
}

func checkPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(strings.Split(strings.Trim(path, "/"), "/")) < 2 {
		return errors.New("unsafe managed path")
	}
	for p := path; p != "/"; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return errors.New("cannot inspect managed path")
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("managed path contains symlink or non-directory")
		}
	}
	return nil
}
func ownedDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed directory is not a real directory")
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("managed directory has unexpected owner")
	}
	if st.Mode().Perm()&0022 != 0 {
		return errors.New("managed directory is writable by other users")
	}
	return nil
}
func secret(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("cannot inspect password file")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4096 {
		return "", errors.New("password file must be regular, private, and bounded")
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return "", errors.New("password file has unexpected owner")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read password file")
	}
	s := strings.TrimSuffix(string(b), "\n")
	s = strings.TrimSuffix(s, "\r")
	if len(s) < 12 || strings.ContainsAny(s, "\x00\r\n") {
		return "", errors.New("password must contain at least 12 characters and no line breaks")
	}
	return s, nil
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func atomicWrite(path string, data []byte) error {
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return errors.New("refusing non-regular file replacement")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".maat-write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }() // Best-effort cleanup; rename removes this path on success.
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func (c *Controller) journalPath() string {
	return filepath.Join(c.cfg.StateDir, "postgres-recovery.json")
}
func (c *Controller) readJournal() (journal, error) {
	var j journal
	st, err := os.Lstat(c.journalPath())
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if !st.Mode().IsRegular() || st.Size() > 16384 {
		return j, errors.New("invalid recovery journal file")
	}
	f, err := os.Open(c.journalPath())
	if err != nil {
		return j, err
	}
	defer func() { _ = f.Close() }() // Read-only journal; decode errors are handled below.
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&j); err != nil {
		return j, errors.New("invalid recovery journal")
	}
	if d.Decode(new(any)) != io.EOF || j.Version != 1 {
		return j, errors.New("unsupported recovery journal")
	}
	switch j.Phase {
	case "primary_ready", "standby_ready", "initializing_primary", "backup_in_progress", "rewind_in_progress", "reinitialization_required", "reinit_prepared", "reinit_retained", "reinit_backup", "reinit_selecting":
	default:
		return j, errors.New("unknown recovery journal phase")
	}
	return j, nil
}
func (c *Controller) writeJournal(j journal) error {
	j.Version = 1
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return atomicWrite(c.journalPath(), b)
}
func (c *Controller) RecoveryState() (string, error) {
	j, err := c.readJournal()
	if j.Phase == "primary_ready" || j.Phase == "standby_ready" {
		return "", err
	}
	return j.Phase, err
}
func (c *Controller) startAllowed() error {
	j, err := c.readJournal()
	if err != nil {
		return err
	}
	if j.Phase != "primary_ready" && j.Phase != "standby_ready" {
		return errors.New("PostgreSQL data has no verified completed preparation; recovery required")
	}
	return nil
}
func (c *Controller) Exists() bool {
	st, err := os.Lstat(filepath.Join(c.cfg.DataDir, "PG_VERSION"))
	return err == nil && st.Mode().IsRegular()
}
func (c *Controller) Empty() (bool, error) {
	e, err := os.ReadDir(c.cfg.DataDir)
	if os.IsNotExist(err) {
		return true, nil
	}
	return len(e) == 0, err
}
func (c *Controller) safeData(path string) error {
	if err := checkPath(path); err != nil {
		return err
	}
	if err := ownedDir(path); err != nil {
		return err
	}
	// All supported files live in the managed directory: no tablespaces or external WAL.
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("data directory contains unsupported symlink")
		}
		if d.Type()&os.ModeType != 0 && !d.IsDir() {
			return errors.New("data directory contains non-regular file")
		}
		if filepath.Base(filepath.Dir(p)) == "pg_tblspc" {
			return errors.New("tablespaces are unsupported")
		}
		return nil
	})
}
func (c *Controller) validateData(path string) error {
	if err := c.safeData(path); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(path, "PG_VERSION"))
	if err != nil || strings.TrimSpace(string(b)) != "18" {
		return errors.New("managed data is not PostgreSQL 18")
	}
	st, err := os.Lstat(filepath.Join(path, "global", "pg_control"))
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("missing PostgreSQL control file")
	}
	return nil
}
func slot(node string) string { return "maat_" + strings.ReplaceAll(node, "-", "_") }
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func connQuote(s string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(s) + "'"
}
func passEscape(s string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(s) }
func (c *Controller) passfile(user string) (string, error) {
	passwordPath := c.cfg.PasswordFile
	if user == "maat_repl" {
		passwordPath = c.cfg.ReplicationPasswordFile
	}
	p, err := secret(passwordPath)
	if err != nil {
		return "", err
	}
	path := filepath.Join(c.cfg.StateDir, user+".pgpass")
	contents := []byte("*:*:*:" + user + ":" + passEscape(p) + "\n")
	if st, e := os.Lstat(path); e == nil && st.Mode().IsRegular() && st.Mode().Perm() == 0600 {
		if previous, e := os.ReadFile(path); e == nil && string(previous) == string(contents) {
			return path, nil
		}
	}
	if err = atomicWrite(path, contents); err != nil {
		return "", err
	}
	return path, nil
}
func (c *Controller) connectionString(u Upstream, user string) (string, error) {
	if err := c.validUpstream(u); err != nil {
		return "", err
	}
	path, err := c.passfile(user)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=postgres passfile=%s sslmode=disable connect_timeout=5 application_name=%s", connQuote(u.Host), u.Port, user, connQuote(path), connQuote(c.cfg.NodeID)), nil
}
func (c *Controller) configure(path string, u *Upstream) error {
	conf := fmt.Sprintf("# Managed by maat.\nlisten_addresses = '*'\nport = %d\nunix_socket_directories = %s\nunix_socket_permissions = 0700\nwal_level = replica\nmax_wal_senders = 10\nmax_replication_slots = 10\nmax_slot_wal_keep_size = '1GB'\nhot_standby = on\nfull_page_writes = on\nwal_log_hints = on\npassword_encryption = 'scram-sha-256'\nlog_statement = 'none'\nlog_min_error_statement = 'panic'\n", c.cfg.Port, sqlQuote(c.socketDir()))
	if err := atomicWrite(filepath.Join(path, "postgresql.conf"), []byte(conf)); err != nil {
		return err
	}
	hba := "local all postgres trust\nlocal all all reject\nhost replication maat_repl 0.0.0.0/0 scram-sha-256\nhost replication maat_repl ::/0 scram-sha-256\nhost all all 0.0.0.0/0 scram-sha-256\nhost all all ::/0 scram-sha-256\n"
	if err := atomicWrite(filepath.Join(path, "pg_hba.conf"), []byte(hba)); err != nil {
		return err
	}
	auto := ""
	if u != nil {
		conn, err := c.connectionString(*u, "maat_repl")
		if err != nil {
			return err
		}
		auto = "primary_conninfo = " + sqlQuote(conn) + "\nprimary_slot_name = " + sqlQuote(slot(c.cfg.NodeID)) + "\nrecovery_target_timeline = 'latest'\n"
	}
	if err := atomicWrite(filepath.Join(path, "postgresql.auto.conf"), []byte(auto)); err != nil {
		return err
	}
	if u != nil {
		return atomicWrite(filepath.Join(path, "standby.signal"), nil)
	}
	return nil
}

func (c *Controller) RecoveryRequestID() (string, error) {
	j, err := c.readJournal()
	return j.RequestID, err
}
