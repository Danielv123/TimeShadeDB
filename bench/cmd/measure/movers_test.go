package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

func writeWindow(t *testing.T, path string, w *core.MoverWindow) {
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

func TestExportMovers(t *testing.T) {
	db := t.TempDir()
	train := core.MoverMeta{ID: "1.2.3", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 2, Kinds: "LC", Color: "ff0000"}
	car := core.MoverMeta{ID: "1.2.v9", Kind: core.KindVehicle, Force: "player", Name: "car"}
	start := core.TimeMs(1_791_630_000_000)
	w := &core.MoverWindow{Kind: core.WindowClosed, Start: start, Len: core.MoverWindowLen,
		Movers: []core.MoverMeta{train, car},
		Rows: []core.MoverRow{
			{Mover: 0, T: start + 100, Tick: 1000, X: 1, Y: 2, Orientation: 0.25},
			{Mover: 0, T: start + 400, Tick: 1018, X: 3, Y: 2, Orientation: 0.25, Flags: core.FlagStop,
				Carriages: []core.Carriage{{X: 3, Y: 2, Orientation: 0.25}, {X: -4, Y: 2, Orientation: 0.25}}},
			{Mover: 0, T: start + 1000, Tick: 1054, Flags: core.FlagGone | core.FlagHandoff},
			{Mover: 1, T: start + 200, Tick: 1006, X: 5, Y: 5},
		}}
	sfc := filepath.Join(db, "movers", "m.nauvis")
	writeWindow(t, filepath.Join(sfc, "windows", "2026-10-09", "1791630000000.mvw"), w)
	// The next window: imported rows (no tick, no heading) after the handoff.
	w2 := &core.MoverWindow{Kind: core.WindowClosed, Start: start + core.MoverWindowLen, Len: core.MoverWindowLen,
		Movers: []core.MoverMeta{train},
		Rows:   []core.MoverRow{{Mover: 0, T: start + core.MoverWindowLen + 5, X: 9, Y: 9, Flags: core.FlagNoTick | core.FlagNoHeading}}}
	writeWindow(t, filepath.Join(sfc, "windows", "2026-10-09", "1791630300000.mvw"), w2)
	// Keyframes repeat rows and are not read.
	kf := *w
	kf.Kind, kf.Len = core.WindowKeyframe, 0
	writeWindow(t, filepath.Join(sfc, "keyframes", "2026-10-09", "1791630000000.mvw"), &kf)

	ex, err := exportMovers(db, "m.nauvis", core.KindTrain)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Windows != 2 || ex.Rows != 4 || len(ex.Journey) != 1 {
		t.Fatalf("windows %d rows %d journeys %d, want 2, 4, 1", ex.Windows, ex.Rows, len(ex.Journey))
	}
	j := ex.Journey[0]
	if j.ID != "1.2.3" || j.Name != "locomotive" || len(j.Metas) != 1 || j.Metas[0] != [3]any{2, "LC", "ff0000"} {
		t.Fatalf("journey %+v", j)
	}
	r := j.Rows
	if r[0][0] != int64(start+100) || r[0][1] != int64(1000) || r[0][4] != 0.25 {
		t.Errorf("row 0 %v", r[0])
	}
	if cs, ok := r[1][7].([][3]float64); !ok || len(cs) != 2 || cs[1][0] != -4 {
		t.Errorf("row 1 carriages %v", r[1][7])
	}
	if r[2][5] != int(core.FlagGone|core.FlagHandoff) || r[2][4] != nil {
		t.Errorf("gone row %v", r[2])
	}
	if r[3][1] != nil || r[3][4] != nil {
		t.Errorf("imported row %v: tick and heading should be null", r[3])
	}
}
