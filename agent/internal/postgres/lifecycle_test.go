package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run explicitly under an unprivileged user with PostgreSQL 18 tools on PATH:
// MAAT_PG_INTEGRATION=1 go test ./internal/postgres -run TestNativeLifecycle -v
func TestNativeLifecycle(t *testing.T) {
	if os.Getenv("MAAT_PG_INTEGRATION") != "1" {
		t.Skip("requires PostgreSQL 18 native tools and an unprivileged OS user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	aCfg := configForTest(t)
	aCfg.Port = 16541
	a, err := New(aCfg)
	if err != nil {
		t.Fatal(err)
	}
	bCfg := configForTest(t)
	bCfg.Port = 16542
	bCfg.NodeID = "b"
	b, err := New(bCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Stop(context.Background()); _ = b.Stop(context.Background()) }()
	if err = a.InitializePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if running, err := a.running(ctx); err != nil || running {
		t.Fatalf("initialization started database: %v %v", running, err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oa, err := a.Observe(ctx)
	if err != nil || !oa.Healthy || oa.Recovery {
		t.Fatalf("primary: %+v %v", oa, err)
	}
	ua := Upstream{Host: "127.0.0.1", Port: aCfg.Port, NodeID: "a", SystemID: oa.SystemID}
	if err = b.InitializeReplica(ctx, ua); err != nil {
		t.Fatal(err)
	}
	if err = b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, b, ua)
	// Repeat reconciliation without changing or restarting an already healthy replica.
	if err = b.Follow(ctx, ua); err != nil {
		t.Fatal(err)
	}
	if err = b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// Removing a standby signal without promotion must never admit writes.
	signal := filepath.Join(bCfg.DataDir, "standby.signal")
	if err = os.Remove(signal); err != nil {
		t.Fatal(err)
	}
	if err = b.RecoverAuthorizedPromotion(ctx, oa.SystemID, oa.Timeline, oa.FlushLSN); err == nil {
		t.Fatal("unpromoted standby admitted after out-of-band signal removal")
	}
	if running, e := b.running(ctx); e != nil || running {
		t.Fatalf("unsafe recovery started server: %v %v", running, e)
	}
	blocked, e := b.readJournal()
	if e != nil || blocked.Phase != "standby_ready" {
		t.Fatalf("unsafe recovery changed journal: %+v %v", blocked, e)
	}
	if err = atomicWrite(signal, nil); err != nil {
		t.Fatal(err)
	}
	if err = b.Follow(ctx, ua); err != nil {
		t.Fatalf("prepare stopped standby: %v", err)
	}
	if err = b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, b, ua)
	conn, err := a.connect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "CREATE TABLE maat_check (value text); INSERT INTO maat_check VALUES ('replicated')"); err != nil {
		t.Fatal(err)
	}
	closeConn(conn)
	awaitReplica(t, ctx, b, ua)
	conn, err = b.connect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err = conn.QueryRow(ctx, "SELECT value FROM maat_check").Scan(&value); err != nil || value != "replicated" {
		t.Fatalf("replicated value=%q err=%v", value, err)
	}
	closeConn(conn)
	if err = run(ctx, "pg_ctl", "-D", aCfg.DataDir, "-m", "immediate", "-w", "-t", "10", "stop"); err != nil {
		t.Fatal(err)
	}
	// Reproduce a crash after PostgreSQL promotes but before the agent journal
	// and replication slots are updated. EnsurePrimary finishes that boundary.
	conn, err = b.connect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "SELECT pg_promote(true,30)"); err != nil {
		t.Fatal(err)
	}
	closeConn(conn)
	before, err := b.readJournal()
	if err != nil || before.Phase != "standby_ready" {
		t.Fatalf("unexpected pre-recovery journal: %+v %v", before, err)
	}
	// Also lose the server before the journal update: ordinary Start must
	// reject missing standby.signal until guarded promotion recovery verifies it.
	if err = run(ctx, "pg_ctl", "-D", bCfg.DataDir, "-m", "immediate", "-w", "-t", "10", "stop"); err != nil {
		t.Fatal(err)
	}
	if err = b.Start(ctx); err == nil {
		t.Fatal("ordinary Start admitted ambiguous promoted data")
	}
	if err = b.RecoverAuthorizedPromotion(ctx, "1", oa.Timeline, oa.FlushLSN); err == nil {
		t.Fatal("accepted wrong promotion system identity")
	}
	if err = b.RecoverAuthorizedPromotion(ctx, oa.SystemID, uint64(^uint32(0)), oa.FlushLSN); err == nil {
		t.Fatal("accepted insufficient promotion timeline")
	}
	if err = b.RecoverAuthorizedPromotion(ctx, oa.SystemID, oa.Timeline, ^uint64(0)); err == nil {
		t.Fatal("accepted checkpoint below promotion watermark")
	}
	rejected, e := b.readJournal()
	if e != nil || rejected.Phase != "standby_ready" {
		t.Fatalf("rejected proof changed journal: %+v %v", rejected, e)
	}
	if err = b.RecoverAuthorizedPromotion(ctx, oa.SystemID, oa.Timeline, oa.FlushLSN); err != nil {
		t.Fatalf("recover stopped promoted candidate: %v", err)
	}
	if running, e := b.running(ctx); e != nil || running {
		t.Fatalf("guarded recovery opened server: %v %v", running, e)
	}
	if err = b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.EnsurePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := b.readJournal()
	if err != nil || after.Phase != "primary_ready" {
		t.Fatalf("promotion journal not recovered: %+v %v", after, err)
	}
	if err = b.Promote(ctx); err != nil {
		t.Fatalf("idempotent promotion: %v", err)
	}
	ob, err := b.Observe(ctx)
	if err != nil || ob.Recovery || ob.Timeline <= oa.Timeline {
		t.Fatalf("promotion: %+v %v", ob, err)
	}
	ub := Upstream{Host: "127.0.0.1", Port: bCfg.Port, NodeID: "b", SystemID: ob.SystemID}
	if err = a.Rewind(ctx, ub); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, a, ub)
	if err = a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.Reinitialize(ctx, ub, "test_rebuild_1"); err != nil {
		t.Fatal(err)
	}
	if request, e := a.RecoveryRequestID(); e != nil || request != "test_rebuild_1" {
		t.Fatalf("recovery request identity: %q %v", request, e)
	}
	retained := filepath.Join(filepath.Dir(aCfg.DataDir), filepath.Base(aCfg.DataDir)+".retained-test_rebuild_1")
	if _, err = os.Stat(filepath.Join(retained, "PG_VERSION")); err != nil {
		t.Fatalf("old data not retained: %v", err)
	}
	if err = a.Reinitialize(ctx, ub, "test_rebuild_1"); err != nil {
		t.Fatalf("rebuild retry: %v", err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, a, ub)
	j, err := a.readJournal()
	if err != nil || j.Phase != "standby_ready" {
		t.Fatalf("journal: %+v %v", j, err)
	}
	// Simulate a failed rebuild after the original data was retained. A new,
	// explicit request can rebuild without deleting either preserved copy.
	if err = a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	failedRetained := aCfg.DataDir + ".retained-failed_request"
	if err = os.Rename(aCfg.DataDir, failedRetained); err != nil {
		t.Fatal(err)
	}
	failedStaging := aCfg.DataDir + ".new-failed_request"
	if err = os.Mkdir(failedStaging, 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.writeJournal(journal{Phase: "reinitialization_required", RequestID: "failed_request", Upstream: ub, SystemID: ub.SystemID, Retained: failedRetained, Staging: failedStaging}); err != nil {
		t.Fatal(err)
	}
	if err = a.Reinitialize(ctx, ub, "new_operator_request"); err != nil {
		t.Fatalf("new request after failed backup: %v", err)
	}
	if _, err = os.Stat(filepath.Join(failedRetained, "PG_VERSION")); err != nil {
		t.Fatalf("previous retained data lost: %v", err)
	}
	if _, err = os.Stat(failedStaging); err != nil {
		t.Fatalf("partial backup lost: %v", err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, a, ub)
	// Both sides of the durable directory-selection rename are resumable.
	if err = a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	selection, err := a.readJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(aCfg.DataDir, selection.Staging); err != nil {
		t.Fatal(err)
	}
	selection.Phase = "reinit_selecting"
	if err = a.writeJournal(selection); err != nil {
		t.Fatal(err)
	}
	if err = a.Reinitialize(ctx, ub, selection.RequestID); err != nil {
		t.Fatalf("resume before selection rename: %v", err)
	}
	if err = a.writeJournal(selection); err != nil {
		t.Fatal(err)
	}
	if err = a.Reinitialize(ctx, ub, selection.RequestID); err != nil {
		t.Fatalf("resume after selection rename: %v", err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, a, ub)
	// A running primary cannot be the local replacement target.
	if err = b.Reinitialize(ctx, ua, "primary_target"); err == nil {
		t.Fatal("accepted running primary replacement")
	}
	if err = a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"prepared", "renamed", "retained", "backup_complete", "backup_partial"} {
		t.Run("rebuild_crash_"+stage, func(t *testing.T) {
			request := "crash_" + stage
			j := journal{Phase: "reinit_prepared", RequestID: request, Upstream: ub, SystemID: ub.SystemID, Retained: aCfg.DataDir + ".retained-" + request, Staging: aCfg.DataDir + ".new-" + request}
			if stage != "prepared" {
				if err := os.Rename(aCfg.DataDir, j.Retained); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "retained" {
				j.Phase = "reinit_retained"
			}
			if stage == "backup_complete" {
				j.Phase = "reinit_backup"
				if err := a.backup(ctx, ub, j.Staging); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "backup_partial" {
				j.Phase = "reinit_backup"
				if err := os.Mkdir(j.Staging, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.writeJournal(j); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(aCfg)
			if err != nil {
				t.Fatal(err)
			}
			a = restarted
			if err = a.Start(ctx); err == nil {
				t.Fatal("interrupted filesystem phase permitted startup")
			}
			before, err := os.ReadFile(a.journalPath())
			if err != nil {
				t.Fatal(err)
			}
			changed := ub
			changed.NodeID = "c"
			if err = a.Reinitialize(ctx, changed, request); err == nil {
				t.Fatal("changed source admitted during same request")
			}
			after, err := os.ReadFile(a.journalPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("changed source mutated recovery journal")
			}
			err = a.Reinitialize(ctx, ub, request)
			switch stage {
			case "backup_complete":
				if err == nil {
					t.Fatal("completed backup resumed without authority revalidation boundary")
				}
				state, e := a.RecoveryState()
				if e != nil || state != "reinit_selecting" {
					t.Fatalf("completed backup phase=%q err=%v", state, e)
				}
				if err = a.Reinitialize(ctx, ub, request); err != nil {
					t.Fatal(err)
				}
			case "backup_partial":
				if err == nil {
					t.Fatal("incomplete backup was selected")
				}
				state, e := a.RecoveryState()
				if e != nil || state != "reinitialization_required" {
					t.Fatalf("incomplete backup phase=%q err=%v", state, e)
				}
				if id, e := a.RecoveryRequestID(); e != nil || id != request {
					t.Fatalf("failed request identity=%q err=%v", id, e)
				}
				if err = a.Reinitialize(ctx, ub, request); err == nil {
					t.Fatal("failed request automatically repeated replacement")
				}
				if err = a.Reinitialize(ctx, ub, request+"_retry"); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(j.Staging); err != nil {
					t.Fatal("partial backup was discarded")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = os.Stat(filepath.Join(j.Retained, "PG_VERSION")); err != nil {
				t.Fatalf("retained original missing: %v", err)
			}
			if err = a.Start(ctx); err != nil {
				t.Fatal(err)
			}
			awaitReplica(t, ctx, a, ub)
			if err = a.Stop(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReplica(t, ctx, a, ub)
	for _, c := range []*Controller{a, b} {
		for _, path := range []string{c.journalPath(), filepath.Join(c.cfg.DataDir, "postgresql.auto.conf")} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "test-only-password") {
				t.Fatal("secret in configuration/journal")
			}
		}
	}
}
func awaitReplica(t *testing.T, ctx context.Context, c *Controller, u Upstream) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = c.VerifyReplica(ctx, u); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal(err)
}
