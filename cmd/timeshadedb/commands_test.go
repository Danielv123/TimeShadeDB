package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommandRegistryHasLegacyCommands(t *testing.T) {
	for _, name := range []string{
		"import-csv", "query-tile", "stats", "verify", "inspect-index", "export-tile", "compact",
		"copy-chunks", "benchmark", "serve", "tail-chunk-tsv", "tail-entity-tsv",
	} {
		c, ok := lookupCommand(name)
		if !ok {
			t.Fatalf("command %q is not registered", name)
		}
		if c.Group != GroupLegacy {
			t.Fatalf("command %q group = %q, want %q", name, c.Group, GroupLegacy)
		}
	}
}

func TestRunCommandUsageAndUnknown(t *testing.T) {
	var out bytes.Buffer
	if err := runCommand(context.Background(), &out, []string{"help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "legacy commands:") || !strings.Contains(out.String(), "serve") {
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
		{Name: "Bad_Name", Group: GroupV3, Run: noop},
		{Name: "no-run", Group: GroupV3},
		{Name: "no-group", Run: noop},
		{Name: "serve", Group: GroupLegacy, Run: noop},
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
