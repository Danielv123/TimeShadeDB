package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"timeshadedb/internal/v3/core"
)

func TestReplicate(t *testing.T) {
	src := t.TempDir()
	ds := core.DatastoreKey{Map: "gw-a.1", Surface: "nauvis", Force: "player"}
	sfc := core.SurfaceKey{Map: "gw-a.1", Surface: "nauvis"}
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "timeshadedb-v3.json"), `{"format":"timeshadedb-v3","version":1}`)
	write(filepath.Join(src, "datastores", ds.ID(), "meta.json"), `{"key":{"map":"gw-a.1","surface":"nauvis","force":"player"},"window_ms":60000}`)
	write(filepath.Join(src, "datastores", ds.ID(), "tiles", "0_0.blk"), "blocks")
	write(filepath.Join(src, "overlays", ds.ID(), "log.ivl"), "log")
	write(filepath.Join(src, "movers", sfc.ID(), "meta.json"), `{"key":{"map":"gw-a.1","surface":"nauvis"}}`)
	write(filepath.Join(src, "movers", "wal", "0001.wal"), "wal")
	dst := filepath.Join(t.TempDir(), "x3")
	if err := replicate(src, dst, 3); err != nil {
		t.Fatal(err)
	}
	for k, suffix := range []string{"", "-r1", "-r2"} {
		d := core.DatastoreKey{Map: ds.Map + suffix, Surface: ds.Surface, Force: ds.Force}
		b, err := os.ReadFile(filepath.Join(dst, "datastores", d.ID(), "meta.json"))
		if err != nil {
			t.Fatalf("copy %d: %v", k, err)
		}
		var m struct {
			Key core.DatastoreKey `json:"key"`
			W   int               `json:"window_ms"`
		}
		if err := json.Unmarshal(b, &m); err != nil || m.Key != d || m.W != 60000 {
			t.Fatalf("copy %d meta %s (%v)", k, b, err)
		}
		if _, err := os.Stat(filepath.Join(dst, "datastores", d.ID(), "tiles", "0_0.blk")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "overlays", d.ID(), "log.ivl")); err != nil {
			t.Fatal(err)
		}
		s := core.SurfaceKey{Map: sfc.Map + suffix, Surface: sfc.Surface}
		if _, err := os.Stat(filepath.Join(dst, "movers", s.ID(), "meta.json")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "movers", "wal", "0001.wal")); err != nil {
		t.Fatal("the movers WAL is copied once:", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "timeshadedb-v3.json")); err != nil {
		t.Fatal(err)
	}
}
