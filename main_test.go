package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandRequiresExplicitValidConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"run"}, {"reinitialize", "-node", "b", "-generation", "1"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted missing/invalid configuration: %v", args)
		}
	}
}

func TestConfiguredNodeAppearsInLogs(t *testing.T) {
	config, err := os.ReadFile("deploy/a.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var out bytes.Buffer
	_ = run(context.Background(), []string{"status", "--config", path}, &out)
	slog.Warn("probe")
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["host"] != "a" {
		t.Fatalf("log host = %v, want a", record["host"])
	}
}
