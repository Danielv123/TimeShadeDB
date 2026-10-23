package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"timeshadedb/internal/import/minimap/minimaptest"
	"timeshadedb/internal/v3/core"
)

func TestImportMinimapCommand(t *testing.T) {
	c, ok := lookupCommand("import-minimap")
	if !ok || c.Group != GroupV3 {
		t.Fatalf("import-minimap not registered as a v3 command: %+v", c)
	}
	exp := minimaptest.Write(t, t.TempDir())
	out := t.TempDir()
	reportPath := filepath.Join(out, "report.json")
	mapsPath := filepath.Join(out, "maps.json")
	var buf bytes.Buffer
	err := runImportMinimap(context.Background(), &buf, []string{
		"--verify", "--verify-history", "-1", "--report", reportPath, "--maps-out", mapsPath, exp.Root,
	})
	if err != nil {
		t.Fatalf("import-minimap: %v\n%s", err, buf.String())
	}
	for _, want := range []string{
		"verify terrain: final 2/2 areas equal",
		"history 1/1, coarse z1-z4 2/2",
		"verify movers gw-test~2E1.nauvis: match=true",
		"verify overlays gw-test~2E1.nauvis.player: match=true intervals=3",
		"terrain store: {Accepted:7 Late:0 Invalid:0}",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
	var report struct {
		Import struct {
			Movers []struct {
				Retimed map[string]int `json:"retimed"`
			} `json:"movers"`
		} `json:"import"`
		TerrainVerify struct {
			FinalEqual int `json:"final_equal"`
		} `json:"terrain_verify"`
	}
	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.TerrainVerify.FinalEqual != 2 || len(report.Import.Movers) != 1 || report.Import.Movers[0].Retimed["t"] != 2 {
		t.Fatalf("report: %s", b)
	}
	var maps []core.MapInfo
	b, err = os.ReadFile(mapsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &maps); err != nil || len(maps) != 1 || maps[0].Key != "gw-test.1" {
		t.Fatalf("maps-out: %s (%v)", b, err)
	}

	// The data directory may also come from --data.
	buf.Reset()
	if err := runImportMinimap(context.Background(), &buf, []string{"--data", exp.Root, "--no-movers", "--no-overlays"}); err != nil {
		t.Fatalf("--data: %v", err)
	}
}

func TestImportMinimapCommandErrors(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	for _, args := range [][]string{
		nil,
		{"a", "b"},
		{"--data", "a", "b"},
		{"--window", "0s", "x"},
		{"--no-such-flag"},
		{filepath.Join(t.TempDir(), "missing")},
	} {
		if err := runImportMinimap(ctx, &buf, args); err == nil {
			t.Errorf("args %q: no error", args)
		}
	}
	buf.Reset()
	if err := runImportMinimap(ctx, &buf, []string{"-h"}); err != nil || !strings.Contains(buf.String(), "usage: timeshadedb import-minimap") {
		t.Fatalf("help: %v\n%s", err, buf.String())
	}
}

func TestImportMinimapCommandFailsOnCorruptFiles(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	p := filepath.Join(exp.Root, "gw-test~2E1", "nauvis", "m", "1791630300001.m.gz")
	if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := runImportMinimap(context.Background(), &buf, []string{exp.Root})
	if err == nil || !strings.Contains(err.Error(), "1 files failed") {
		t.Fatalf("corrupt window file: err = %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "error: ") {
		t.Errorf("output does not list the error:\n%s", buf.String())
	}
	buf.Reset()
	if err := runImportMinimap(context.Background(), &buf, []string{"--allow-errors", exp.Root}); err != nil {
		t.Fatalf("--allow-errors: %v\n%s", err, buf.String())
	}
}
