package benchlib

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

func writeMVW(t *testing.T, path string, w *core.MoverWindow) {
	t.Helper()
	b, err := wire.EncodeMoverWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	z := enc.EncodeAll(b, nil)
	enc.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, z, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCountV3MoverRows(t *testing.T) {
	db := t.TempDir()
	w := &core.MoverWindow{
		Surface: core.SurfaceKey{Map: "m", Surface: "nauvis"},
		Kind:    core.WindowClosed,
		Start:   1_791_630_000_000,
		Len:     core.MoverWindowLen,
		Movers:  []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Force: "player", Name: "locomotive"}},
		Rows: []core.MoverRow{
			{Mover: 0, T: 1_791_630_000_100, Tick: 1000, X: 1, Y: 2},
			{Mover: 0, T: 1_791_630_000_400, Tick: 1018, X: 3, Y: 2},
			{Mover: 0, T: 1_791_630_001_000, Tick: 1054, Flags: core.FlagGone},
		},
	}
	sfc := filepath.Join(db, "movers", "m.nauvis")
	writeMVW(t, filepath.Join(sfc, "windows", "2026-10-09", "1791630000000.mvw"), w)
	w2 := *w
	w2.Start += core.MoverWindowLen
	w2.Rows = []core.MoverRow{{Mover: 0, T: w2.Start + 5, Tick: 2000, X: 9, Y: 9}}
	writeMVW(t, filepath.Join(sfc, "windows", "2026-10-09", "1791630300000.mvw"), &w2)
	// A keyframe repeats rows and must not be counted.
	kf := *w
	kf.Kind, kf.Len = core.WindowKeyframe, 0
	writeMVW(t, filepath.Join(sfc, "keyframes", "2026-10-09", "1791630000000.mvw"), &kf)

	c, err := CountV3MoverRows(db)
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows != 2 || c.Samples != 3 || c.Gones != 1 {
		t.Fatalf("counted %+v, want 2 windows, 3 samples, 1 gone", c)
	}
	if err := os.MkdirAll(filepath.Join(db, "icons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "icons", "a.png"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := MeasureV3Disk(db)
	if err != nil || d.Movers == 0 {
		t.Fatalf("mover bytes %d %v", d.Movers, err)
	}
	if d.Icons != 100 || d.Other != 0 {
		t.Fatalf("icons %d other %d, want 100 and 0", d.Icons, d.Other)
	}
	if c, err := CountV3MoverRows(t.TempDir()); err != nil || c.Windows != 0 {
		t.Fatalf("empty db: %+v %v", c, err)
	}
}

func TestRedactPath(t *testing.T) {
	data := filepath.Join(t.TempDir(), "minimap")
	t.Setenv("TSDB_TESTDATA", data)
	t.Setenv("TSDB_WORK", "")
	cases := map[string]string{
		data:                                   "$TSDB_TESTDATA",
		filepath.Join(data, "gw", "x.tsv"):     "$TSDB_TESTDATA/gw/x.tsv",
		filepath.Join(os.TempDir(), "a", "db"): "$TMP/a/db",
		"relative/path":                        "relative/path",
		"/api/v3/ingest":                       "/api/v3/ingest",
		"":                                     "",
	}
	if filepath.Separator == '\\' {
		cases[`Q:\elsewhere\stream\gw.chunks.tsv`] = "gw.chunks.tsv"
	} else {
		cases["/srv/elsewhere/gw.chunks.tsv"] = "gw.chunks.tsv"
		delete(cases, "/api/v3/ingest")
	}
	for in, want := range cases {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
	r := NewResult("x", "l", "v3")
	r.Param("db", filepath.Join(os.TempDir(), "db"))
	r.Param("input", []string{filepath.Join(data, "a.tsv")})
	r.Param("command", "bin import --data "+data+" --db x")
	r.Note("POST /api/v3/ingest failed for %s", filepath.Join(data, "a.tsv"))
	if err := r.Write(""); err != nil {
		t.Fatal(err)
	}
	if r.Params["db"] != "$TMP/db" || r.Params["input"].([]string)[0] != "$TSDB_TESTDATA/a.tsv" ||
		r.Params["command"] != "bin import --data $TSDB_TESTDATA --db x" ||
		r.Notes[0] != "POST /api/v3/ingest failed for $TSDB_TESTDATA/a.tsv" {
		t.Fatalf("params %v notes %v", r.Params, r.Notes)
	}
}
