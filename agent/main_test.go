package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maat/internal/agent"
)

type probeCommand struct {
	handler func(context.Context, agent.Config, agent.ReinitializeRequest, io.Writer) error
}

func (probeCommand) name() string { return "probe" }
func (c probeCommand) execute(ctx context.Context, config agent.Config, request agent.ReinitializeRequest, out io.Writer) error {
	return c.handler(ctx, config, request, out)
}

func TestNewCommandDefinitionDrivesUsageAndDispatch(t *testing.T) {
	previous, logger := commands, slog.Default()
	t.Cleanup(func() { commands = previous; slog.SetDefault(logger) })
	want := errors.New("command result")
	ctx := context.Background()
	var out bytes.Buffer
	commands = append(commands, probeCommand{handler: func(gotCtx context.Context, config agent.Config, request agent.ReinitializeRequest, writer io.Writer) error {
		if gotCtx != ctx || config.ID != "instance-a" || writer != &out || request != (agent.ReinitializeRequest{NodeID: "instance-b", Generation: 2, Acknowledge: true}) {
			t.Fatal("command did not receive parsed arguments and runtime context")
		}
		return want
	}})
	if err := run(ctx, nil, &out); err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("usage omitted registered command: %v", err)
	}
	args := []string{"probe", "--config", "deploy/instance-a.json", "--node", "instance-b", "--generation", "2", "--ack-data-replacement"}
	if err := run(ctx, args, &out); !errors.Is(err, want) {
		t.Fatalf("registered handler result = %v, want %v", err, want)
	}
}

func TestCommandRequiresExplicitValidConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"run"}, {"reinitialize", "-node", "b", "-generation", "1"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted missing/invalid configuration: %v", args)
		}
	}
}

func TestReinitializeRequiresTargetGenerationAndAcknowledgement(t *testing.T) {
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	for _, flags := range [][]string{
		{"--generation", "2", "--ack-data-replacement"},
		{"--node", "instance-b", "--ack-data-replacement"},
		{"--node", "instance-b", "--generation", "2"},
	} {
		args := append([]string{"reinitialize", "--config", "deploy/instance-a.json"}, flags...)
		err := run(context.Background(), args, io.Discard)
		if err == nil || err.Error() != "reinitialize requires --node, --generation, and --ack-data-replacement" {
			t.Fatalf("reinitialize %v did not reject incomplete recovery request: %v", flags, err)
		}
	}
}

func TestConfiguredNodeAppearsInLogs(t *testing.T) {
	config, err := os.ReadFile("deploy/instance-a.json")
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
	if record["host"] != "instance-a" {
		t.Fatalf("log host = %v, want instance-a", record["host"])
	}
}
