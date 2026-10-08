//go:build maat_faults

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFaultIsOneShotAndCancellable(t *testing.T) {
	dir := t.TempDir()
	a := &Runtime{cfg: Config{ID: "b", StateDir: dir}}
	if err := os.WriteFile(filepath.Join(dir, "fault-arm"), []byte("before-promote"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.fault(context.Background(), "after-promote"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.fault(ctx, "before-promote"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fault did not block: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fault-reached")); err != nil {
		t.Fatal(err)
	}
	if err := a.fault(context.Background(), "before-promote"); err != nil {
		t.Fatal(err)
	}
}
