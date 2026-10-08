package main

import (
	"context"
	"errors"
	"io"

	"maat/internal/agent"
)

type command interface {
	name() string
	execute(context.Context, agent.Config, agent.ReinitializeRequest, io.Writer) error
}

// Register commands here; usage, validation, and dispatch all use this list.
var commands = []command{runCommand{}, statusCommand{}, reinitializeCommand{}}

func allCommandNames() []string {
	names := make([]string, 0, len(commands))

	for _, candidate := range commands {
		names = append(names, candidate.name())
	}

	return names
}

func selectCommandByName(command_name string) command {

	var selected command

	for _, candidate := range commands {
		if command_name == candidate.name() {
			selected = candidate
		}
	}

	return selected
}

// / run
type runCommand struct{}

func (runCommand) name() string { return "run" }

func (runCommand) execute(ctx context.Context, config agent.Config, _ agent.ReinitializeRequest, _ io.Writer) error {
	return agent.Run(ctx, config)
}

// status
type statusCommand struct{}

func (statusCommand) name() string { return "status" }

func (statusCommand) execute(ctx context.Context, config agent.Config, _ agent.ReinitializeRequest, out io.Writer) error {
	return agent.Control(ctx, config, "/status", nil, out)
}

// reinitialize
type reinitializeCommand struct{}

func (reinitializeCommand) name() string { return "reinitialize" }

func (c reinitializeCommand) execute(ctx context.Context, config agent.Config, request agent.ReinitializeRequest, out io.Writer) error {
	if request.NodeID == "" || request.Generation == 0 || !request.Acknowledge {
		return errors.New(c.name() + " requires --node, --generation, and --ack-data-replacement")
	}
	return agent.Control(ctx, config, "/reinitialize", request, out)
}
