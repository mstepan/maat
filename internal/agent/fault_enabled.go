//go:build maat_faults

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fault is a one-shot integration-test handshake. Only the reconciliation
// goroutine waits: the runner can independently stop or kill the whole agent.
// Production builds contain no filesystem hooks.
func (a *Runtime) fault(ctx context.Context, point string) error {
	path := filepath.Join(a.cfg.StateDir, "fault-arm")
	armed, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(armed)) != point {
		return nil
	}
	if e = os.Remove(path); e != nil {
		return e
	}
	marker, _ := json.Marshal(map[string]any{"point": point, "pid": os.Getpid(), "node_id": a.cfg.ID})
	if e = os.WriteFile(filepath.Join(a.cfg.StateDir, "fault-reached.tmp"), marker, 0600); e != nil {
		return e
	}
	if e = os.Rename(filepath.Join(a.cfg.StateDir, "fault-reached.tmp"), filepath.Join(a.cfg.StateDir, "fault-reached")); e != nil {
		return e
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, e = os.Stat(filepath.Join(a.cfg.StateDir, "fault-release")); e == nil {
			return nil
		} else if !os.IsNotExist(e) {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
