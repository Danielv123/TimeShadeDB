package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommandRegistry(t *testing.T) {
	for _, name := range []string{"serve", "tail", "import-minimap", "publish", "publish-serve"} {
		if _, ok := lookupCommand(name); !ok {
			t.Fatalf("command %q is not registered", name)
		}
	}
}

func TestRunCommandUsageAndUnknown(t *testing.T) {
	var out bytes.Buffer
	if err := runCommand(context.Background(), &out, []string{"help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "commands:") || !strings.Contains(out.String(), "serve") {
		t.Fatalf("usage text missing commands:\n%s", out.String())
	}
	if err := runCommand(context.Background(), &out, nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("empty args error = %v", err)
	}
	if err := runCommand(context.Background(), &out, []string{"no-such-command"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestRegisterRejectsBadCommands(t *testing.T) {
	noop := func(context.Context, []string) error { return nil }
	for _, c := range []Command{
		{Name: "Bad_Name", Run: noop},
		{Name: "no-run"},
		{Name: "serve", Run: noop},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("register(%+v) did not panic", c.Name)
				}
			}()
			register(c)
		}()
	}
}
