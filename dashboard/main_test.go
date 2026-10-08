package main

import (
	"io"
	"testing"
)

func TestProjectFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{{nil, "maat-dev"}, {[]string{"--project", "integration-lab"}, "integration-lab"}} {
		got, err := options(test.args, io.Discard)
		if err != nil || got != test.want {
			t.Fatalf("options=%q %v", got, err)
		}
	}
	if _, err := options([]string{"unexpected"}, io.Discard); err == nil {
		t.Fatal("positional argument accepted")
	}
}
