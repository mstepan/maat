package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLSNStrict(t *testing.T) {
	for input, want := range map[string]uint64{"0/0": 0, "1/0": 1 << 32, "FFFFFFFF/FFFFFFFF": ^uint64(0), "AB/cd": 0xAB000000cd} {
		got, err := ParseLSN(input)
		if err != nil || got != want {
			t.Fatalf("%q: got %x, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "1", "1/2/3", "-1/2", "0x1/2", "1/100000000", " 1/2", "1/", "+1/1"} {
		if _, err := ParseLSN(input); err == nil {
			t.Fatalf("accepted malformed LSN %q", input)
		}
	}
}

func configForTest(t *testing.T) Config {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "maat-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	base, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin", "replication"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("test-only-password\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Config{DataDir: filepath.Join(base, "data"), StateDir: filepath.Join(base, "state"), PasswordFile: filepath.Join(base, "admin"), ReplicationPasswordFile: filepath.Join(base, "replication"), Port: 5432, NodeID: "a", Peers: []string{"a", "b", "c"}}
}

func TestRejectUnsafePathsAndCredentials(t *testing.T) {
	cfg := configForTest(t)
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.DataDir = "/"
	if _, err := New(bad); err == nil {
		t.Fatal("accepted root data directory")
	}
	bad = cfg
	bad.DataDir = cfg.StateDir
	if _, err := New(bad); err == nil {
		t.Fatal("accepted state/data overlap")
	}
	if err := os.Symlink(cfg.StateDir, cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("accepted symlink data directory")
	}
	if err := os.Remove(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.PasswordFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("accepted public credential file")
	}
}

func TestInterruptedRewindBlocksStartup(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.writeJournal(journal{Version: 1, Phase: "rewind_in_progress"}); err != nil {
		t.Fatal(err)
	}
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, err := c.RecoveryState()
	if err != nil || state != "reinitialization_required" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if err := c.startAllowed(); err == nil {
		t.Fatal("allowed uncertain rewind data to start")
	}
}

func TestRejectUnknownJournalAndTablespaces(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.journalPath(), []byte(`{"version":999,"phase":"ready"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("accepted future journal")
	}
	if err := os.Remove(c.journalPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "pg_tblspc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg.StateDir, filepath.Join(cfg.DataDir, "pg_tblspc", "12345")); err != nil {
		t.Fatal(err)
	}
	if err := c.safeData(cfg.DataDir); err == nil {
		t.Fatal("accepted external tablespace")
	}
}

func TestPreparedDataRequiresJournalAndIdentity(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.startAllowed(); err == nil {
		t.Fatal("unmanaged data may start")
	}
	if err := c.writeJournal(journal{Version: 1, Phase: "reinit_backup", RequestID: "request1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.startAllowed(); err == nil {
		t.Fatal("partial backup may start")
	}
	if err := c.writeJournal(journal{Version: 1, Phase: "standby_ready", SystemID: "1234"}); err != nil {
		t.Fatal(err)
	}
	if err := c.startAllowed(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationUsesDedicatedReplicationAndFiniteWAL(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	u := Upstream{Host: "node-b", Port: 5432, NodeID: "b", SystemID: "12345"}
	if err := c.configure(cfg.DataDir, &u); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "user=maat_repl") || strings.Contains(s, "test-only-password") || !strings.Contains(s, "primary_slot_name = 'maat_a'") {
		t.Fatal("unsafe replication configuration")
	}
	b, err = os.ReadFile(filepath.Join(cfg.DataDir, "postgresql.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "max_slot_wal_keep_size = '1GB'") {
		t.Fatal("unbounded slot retention")
	}
	b, err = os.ReadFile(filepath.Join(cfg.DataDir, "pg_hba.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "host") && !strings.HasSuffix(line, "scram-sha-256") {
			t.Fatal("network authentication is not SCRAM")
		}
	}
}

func TestToolErrorDoesNotExposeOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := run(ctx, "sh", "-c", "echo sensitive-password >&2; exit 1")
	if err == nil || strings.Contains(err.Error(), "sensitive-password") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestReadyRecoveryStateIsNotExclusion(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"primary_ready", "standby_ready"} {
		if err := c.writeJournal(journal{Phase: phase}); err != nil {
			t.Fatal(err)
		}
		state, err := c.RecoveryState()
		if err != nil || state != "" {
			t.Fatalf("healthy phase excluded: %q %v", state, err)
		}
	}
}

func TestRecoveryExclusionSurvivesEveryIncompletePhase(t *testing.T) {
	for _, phase := range []string{"initializing_primary", "backup_in_progress", "rewind_in_progress", "reinitialization_required", "reinit_prepared", "reinit_retained", "reinit_backup", "reinit_selecting"} {
		t.Run(phase, func(t *testing.T) {
			cfg := configForTest(t)
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.writeJournal(journal{Phase: phase, RequestID: "failed_request"}); err != nil {
				t.Fatal(err)
			}
			c, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.startAllowed(); err == nil {
				t.Fatal("incomplete journal permits ordinary startup")
			}
			if request, e := c.RecoveryRequestID(); e != nil || request != "failed_request" {
				t.Fatalf("restart lost request identity: %q %v", request, e)
			}
		})
	}
}

func TestInsufficientSpaceAndSelfTargetDoNotChangeRecoveryState(t *testing.T) {
	cfg := configForTest(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Statfs_t
	if err = syscall.Statfs(filepath.Dir(cfg.DataDir), &st); err != nil {
		t.Fatal(err)
	}
	available := uint64(st.Bavail) * uint64(st.Bsize)
	if err = availableSpace(filepath.Dir(cfg.DataDir), available); err == nil {
		t.Fatal("accepted backup size without required WAL headroom")
	}
	if err = availableSpace(filepath.Dir(cfg.DataDir), ^uint64(0)); err == nil {
		t.Fatal("accepted overflowing backup size")
	}
	if err = c.Reinitialize(context.Background(), Upstream{Host: "localhost", Port: cfg.Port, NodeID: cfg.NodeID, SystemID: "1234"}, "self_target"); err == nil {
		t.Fatal("accepted local node as rebuild source")
	}
	if _, err = os.Stat(c.journalPath()); !os.IsNotExist(err) {
		t.Fatal("rejected recovery changed journal")
	}
	if _, err = os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatal("rejected recovery changed data path")
	}
}
