package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"io"
	"maat/dashboard/internal/agenthttp"
	"maat/dashboard/internal/compose"
	"maat/dashboard/internal/core"
	"maat/dashboard/internal/tui"
	"os"
	"os/signal"
	"syscall"
)

func options(args []string, out io.Writer) (string, error) {
	fs := flag.NewFlagSet("maat-dashboard", flag.ContinueOnError)
	fs.SetOutput(out)
	project := fs.String("project", "maat-dev", "local Compose project")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 || *project == "" {
		return "", errors.New("use --project NAME without positional arguments")
	}
	return *project, nil
}
func run(ctx context.Context, args []string) error {
	project, err := options(args, os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("dashboard requires interactive stdin and stdout terminals")
	}
	adapter := compose.New(project)
	app := core.New(adapter, agenthttp.New(), adapter)
	if err = app.Refresh(ctx); err != nil {
		return err
	}
	return tui.New(app, "Compose project: "+project, nil).Run(ctx)
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	// The foreground Docker/psql child receives Ctrl-C; it must not kill its dashboard parent.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		for {
			select {
			case <-interrupts:
			case <-ctx.Done():
				return
			}
		}
	}()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
