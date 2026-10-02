package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"maat/internal/agent"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: maat {run|status|reinitialize} --config PATH")
	}
	command := args[0]
	if command != "run" && command != "status" && command != "reinitialize" {
		return fmt.Errorf("unknown command %q", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("config", "", "configuration file")
	node := fs.String("node", "", "reinitialization target")
	generation := fs.Uint64("generation", 0, "expected current HA generation")
	ack := fs.Bool("ack-data-replacement", false, "explicitly acknowledge replacing target data while retaining its old directory")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("exactly one --config path and no positional arguments are required")
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Read-only configuration; parse errors are handled below.
	config, err := agent.ReadConfig(f)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.Default().With("host", config.ID))
	switch command {
	case "run":
		return agent.Run(ctx, config)
	case "status":
		return agent.Control(ctx, config, "/status", nil, out)
	case "reinitialize":
		if *node == "" || *generation == 0 || !*ack {
			return errors.New("reinitialize requires --node, --generation, and --ack-data-replacement")
		}
		return agent.Control(ctx, config, "/reinitialize", agent.ReinitializeRequest{NodeID: *node, Generation: *generation, Acknowledge: *ack}, out)
	}
	return nil
}
