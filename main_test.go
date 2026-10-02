package main

import (
	"bytes"
	"context"
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
