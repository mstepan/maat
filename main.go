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
	"strings"
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
		return fmt.Errorf("usage: maat {%s} --config PATH", strings.Join(allCommandNames(), "|"))
	}

	selected := selectCommandByName(args[0])

	if selected == nil {
		return fmt.Errorf("unknown command %q", args[0])
	}
	fs := flag.NewFlagSet(selected.name(), flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("config", "", "configuration file")
	var request agent.ReinitializeRequest
	fs.StringVar(&request.NodeID, "node", "", "reinitialization target")
	fs.Uint64Var(&request.Generation, "generation", 0, "expected current HA generation")
	fs.BoolVar(&request.Acknowledge, "ack-data-replacement", false, "explicitly acknowledge replacing target data while retaining its old directory")
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
	return selected.execute(ctx, config, request, out)
}
