package minimap_test

import (
	"context"
	"math"
	"os"
	"testing"

	"timeshadedb/internal/import/minimap"
	"timeshadedb/internal/import/minimap/minimaptest"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

type stores struct {
	t *memstore.Terrain
	m *memstore.Movers
	o *memstore.Overlays
}

func importSynthetic(t *testing.T, window core.TimeMs) (*minimaptest.Export, *minimap.Dataset, *minimap.Report, stores) {
	t.Helper()
	exp := minimaptest.Write(t, t.TempDir())
	d, err := minimap.Open(exp.Root)
	if err != nil {
		t.Fatal(err)
	}
	s := stores{memstore.NewTerrain(), memstore.NewMovers(), memstore.NewOverlays()}
	rep, err := minimap.Import(context.Background(), d, minimap.Options{
		Terrain: s.t, Movers: s.m, Overlays: s.o, Window: window, Workers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return exp, d, rep, s
}

func TestOpenListsExport(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	d, err := minimap.Open(exp.Root)
	if err != nil {
		t.Fatal(err)
	}
	if d.WindowMs != 300000 || len(d.Datastores) != 1 || len(d.Surfaces) != 1 {
		t.Fatalf("dataset: window %d, %d datastores, %d surfaces", d.WindowMs, len(d.Datastores), len(d.Surfaces))
	}
	src := d.Datastores[0]
	if src.Key != exp.DS || len(src.Areas) != 2 || len(src.Coarse[0]) != 2 || src.Tags == "" || len(src.Recipes) != 1 {
		t.Fatalf("datastore: %+v", src)
	}
	if d.Surfaces[0].Key != exp.Surface || len(d.Surfaces[0].Windows) != 2 || len(d.Surfaces[0].Keyframes) != 1 {
		t.Fatalf("surface: %+v", d.Surfaces[0])
	}
	if len(d.Maps) != 1 {
		t.Fatalf("maps: %+v", d.Maps)
	}
	m := d.Maps[0]
	if m.Key != "gw-test.1" || m.Title != "Gridworld test.1" || m.TileSize != 512 || len(m.Instances) != 2 ||
		m.Instances[0].ID != "1" || m.Instances[0].Bounds == nil || *m.Instances[0].Bounds != (core.Quad{-256, -256, 256, 256}) ||
		len(m.Edges["nauvis"]) != 1 {
		t.Fatalf("map info: %+v", m)
	}
	if _, err := minimap.Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
	if _, err := minimap.Open(t.TempDir()); err == nil {
		t.Fatal("Open of a directory without maps.json.gz succeeded")
	}
}

func TestImportTerrain(t *testing.T) {
	ctx := context.Background()
	exp, d, rep, s := importSynthetic(t, 0)
	if rep.TerrainApply != (core.ApplyResult{Accepted: 7}) {
		t.Fatalf("terrain apply: %+v (reports %+v)", rep.TerrainApply, rep.Terrain[0])
	}
	tr := rep.Terrain[0]
	if tr.Records != 10 || tr.WholeBlocks != 3 || tr.Updates != 7 || tr.Deletes != 1 || tr.Windows != 3 || len(tr.Errors) != 0 {
		t.Fatalf("terrain report: %+v", tr)
	}
	for tile, want := range exp.Final {
		got, st, err := s.t.ReadTile(ctx, exp.DS, tile, core.TimeMs(math.MaxInt64), false)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Fatalf("tile %v final state differs", tile)
		}
		if int(st.Version) != exp.Versions[tile] {
			t.Fatalf("tile %v has %d versions, want %d", tile, st.Version, exp.Versions[tile])
		}
	}
	// Window 1 netted block 0 out, so tile 0,0 changed at the ends of windows 0, 1 and 5.
	hist, err := s.t.History(ctx, exp.DS, core.TileCoord{}, 0, core.TimeMs(math.MaxInt64))
	if err != nil {
		t.Fatal(err)
	}
	T0 := core.TimeMs(minimaptest.T0)
	want := []core.VersionPoint{{T: T0 + 60000, Version: 1}, {T: T0 + 120000, Version: 2}, {T: T0 + 360000, Version: 3}}
	if len(hist) != 3 || hist[0] != want[0] || hist[1] != want[1] || hist[2] != want[2] {
		t.Fatalf("history = %+v, want %+v", hist, want)
	}
	infos, _ := s.t.Datastores(ctx)
	if len(infos) != 1 || infos[0].Horizon != T0+360000 || infos[0].Window != core.DefaultTerrainWindow {
		t.Fatalf("datastore info: %+v", infos)
	}
	tv, err := minimap.VerifyTerrain(ctx, d, s.t, minimap.Options{}, minimap.VerifyOptions{HistoryPerArea: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !tv.OK() || tv.FinalAreas != 2 || tv.HistoryChecks != 1 || tv.CoarseAreas != 2 || tv.LatestSnapshotAreas != 0 {
		t.Fatalf("verify: %+v", tv)
	}
}

func TestImportTerrainWindowDoesNotChangeFinalState(t *testing.T) {
	ctx := context.Background()
	for _, w := range []core.TimeMs{core.Second, 10 * core.Minute} {
		exp, d, _, s := importSynthetic(t, w)
		for tile, want := range exp.Final {
			got, _, err := s.t.ReadTile(ctx, exp.DS, tile, core.TimeMs(math.MaxInt64), false)
			if err != nil || !got.Equal(want) {
				t.Fatalf("window %v tile %v: %v", w, tile, err)
			}
		}
		tv, err := minimap.VerifyTerrain(ctx, d, s.t, minimap.Options{}, minimap.VerifyOptions{HistoryPerArea: -1})
		if err != nil || !tv.OK() {
			t.Fatalf("window %v verify: %+v %v", w, tv, err)
		}
	}
}

func TestVerifyTerrainDetectsDifferences(t *testing.T) {
	ctx := context.Background()
	exp, d, _, s := importSynthetic(t, 0)
	// A later chart update the illisis data does not have.
	px := core.ChunkPixels{}
	px[0] = 0x7777
	T0 := core.TimeMs(minimaptest.T0)
	if _, err := s.t.Apply(ctx, []core.ChunkUpdate{{DS: exp.DS, Chunk: core.ChunkCoord{X: -8, Y: -8}, T: T0 + 400000, Pixels: &px}}); err != nil {
		t.Fatal(err)
	}
	if err := s.t.Commit(ctx, T0+500000); err != nil {
		t.Fatal(err)
	}
	tv, err := minimap.VerifyTerrain(ctx, d, s.t, minimap.Options{}, minimap.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tv.OK() || tv.FinalEqual != 1 || tv.CoarseEqual != 1 || len(tv.Failures) != 2 {
		t.Fatalf("verify should fail on tile 0,0 and its parent: %+v", tv)
	}
}

func TestImportMovers(t *testing.T) {
	ctx := context.Background()
	exp, d, rep, s := importSynthetic(t, 0)
	if len(rep.Movers) != 1 {
		t.Fatalf("mover reports: %+v", rep.Movers)
	}
	mr := rep.Movers[0]
	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("source train samples", mr.Source.Samples["t"], 9)
	check("source player samples", mr.Source.Samples["p"], 4)
	check("source vehicle samples", mr.Source.Samples["v"], 3)
	check("source player gones", mr.Source.Gones["p"], 2)
	check("source dup pairs", mr.SourceDupPairs, 2)
	check("boundary stamped", mr.BoundaryStamped, 2)
	check("retimed trains", mr.Retimed["t"], 2)
	check("dup samples dropped", mr.DupSamplesDropped, 1)
	check("keyframe rows", mr.KeyframeRows, 2)
	check("keyframe matched", mr.KeyframeMatched, 1)
	check("keyframe seeded", mr.KeyframeSeeded, 1)
	check("handoffs", mr.Handoffs, 1)
	check("end switches", mr.EndSwitches, 1)
	check("train jumps before", mr.TrainJumpsBefore, 1)
	check("train jumps after", mr.TrainJumpsAfter, 0)
	check("headings derived", mr.HeadingsDerived, 3)
	check("no heading", mr.NoHeading, 7)
	check("imported train samples", mr.Imported.Samples["t"], 9)
	check("imported train journeys", mr.Imported.Journeys["t"], 2)
	if rep.MoverApply != (core.ApplyResult{Accepted: 18}) {
		t.Fatalf("mover apply: %+v", rep.MoverApply)
	}

	mv, err := minimap.VerifyMovers(ctx, d, rep, s.m)
	if err != nil {
		t.Fatal(err)
	}
	if len(mv) != 1 || !mv[0].Match || mv[0].SourceRows != 18 || mv[0].Matched != 17 || mv[0].MatchedRetimed != 2 ||
		mv[0].SourceDups != 1 || mv[0].Extra != 1 || mv[0].Missing != 0 {
		t.Fatalf("mover verify: %+v vs %+v", mv, mr.Imported)
	}

	// Look at the stored rows.
	T0 := core.TimeMs(minimaptest.T0)
	rows := map[string][]core.MoverRow{}
	for _, start := range []core.TimeMs{T0 - 300000, T0, T0 + 300000} {
		w, err := s.m.Window(ctx, exp.Surface, start)
		if err != nil {
			t.Fatalf("window %v: %v", start, err)
		}
		for _, r := range w.Rows {
			if r.Flags&core.FlagNoTick == 0 {
				t.Fatalf("row without FlagNoTick: %+v", r)
			}
			rows[w.Movers[r.Mover].ID] = append(rows[w.Movers[r.Mover].ID], r)
		}
	}
	train := rows[exp.Train]
	var times []core.TimeMs
	for _, r := range train {
		times = append(times, r.T)
	}
	wantT := []core.TimeMs{T0 + 297000, T0 + 298000, T0 + 299000, core.TimeMs(exp.RetimedTrain[0]), core.TimeMs(exp.RetimedTrain[1]), T0 + 300300, T0 + 300400, core.TimeMs(exp.EndSwitchT)}
	if len(times) != len(wantT) {
		t.Fatalf("train times %v, want %v", times, wantT)
	}
	for i := range wantT {
		if times[i] != wantT[i] {
			t.Fatalf("train times %v, want %v", times, wantT)
		}
	}
	if train[5].X != 33.25 {
		t.Fatalf("duplicate rule kept x=%v, want the later row (33.25)", train[5].X)
	}
	if train[7].Flags&core.FlagEndSwitch == 0 || train[6].Flags&core.FlagStop == 0 || train[0].Orientation != 0.25 {
		t.Fatalf("train flags: %+v", train)
	}
	player := rows[exp.Player]
	if len(player) != 6 || player[2].Flags&core.FlagGone == 0 || player[2].Flags&core.FlagHandoff == 0 ||
		player[4].Flags&core.FlagGone == 0 || player[4].Flags&core.FlagHandoff != 0 || player[0].Flags&core.FlagNoHeading == 0 {
		t.Fatalf("player rows: %+v", player)
	}
	veh := rows[exp.Vehicle]
	if len(veh) != 3 || veh[0].Orientation != 0 || veh[1].Orientation != 0.25 || veh[2].Orientation != 0.25 ||
		veh[2].Flags&core.FlagStop == 0 || veh[0].Flags&core.FlagNoHeading == 0 || veh[1].Flags&core.FlagNoHeading == 0 {
		t.Fatalf("vehicle rows: %+v", veh)
	}
	parked := rows[exp.Parked]
	if len(parked) != 1 || parked[0].T != T0-50000 || parked[0].X != 100 || parked[0].Orientation != 0.5 {
		t.Fatalf("seeded parked train: %+v", parked)
	}
	// The seeded train shows in the first keyframe after it.
	kf, err := s.m.Keyframe(ctx, exp.Surface, T0) // T0 is an hour boundary
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range kf.Rows {
		found = found || kf.Movers[r.Mover].ID == exp.Parked
	}
	if !found {
		t.Fatalf("keyframe lacks the parked train: %+v", kf)
	}
}

func TestImportOverlays(t *testing.T) {
	ctx := context.Background()
	exp, _, rep, s := importSynthetic(t, 0)
	or := rep.Overlays[0]
	if or.TagVersions != 3 || or.RecipeVersions != 1 || or.Empty != 1 || or.Intervals != exp.Intervals || or.Invalid != 0 {
		t.Fatalf("overlay report: %+v", or)
	}
	T0 := core.TimeMs(minimaptest.T0)
	got, err := s.o.Query(ctx, exp.DS, core.OverlayQuery{Tile: core.TileCoord{}, T0: T0, T1: T0 + core.Day})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("intervals: %+v", got)
	}
	// Sorted by (kind, key, start): the recipe, then the two tag versions.
	if got[0].Kind != core.OverlayRecipe || got[0].Key != "1/55" || got[0].Recipe != "iron-plate" || !got[0].Open() ||
		got[1].Text != "a" || got[1].End != T0+5000 || got[2].Text != "b" || !got[2].Open() || got[2].Start != T0+5000 {
		t.Fatalf("intervals: %+v", got)
	}
	ov, err := minimap.VerifyOverlays(ctx, rep, s.o)
	if err != nil || len(ov) != 1 || !ov[0].Match {
		t.Fatalf("overlay read-back: %+v %v", ov, err)
	}
}

func TestImportFilters(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	d, err := minimap.Open(exp.Root)
	if err != nil {
		t.Fatal(err)
	}
	ter := memstore.NewTerrain()
	rep, err := minimap.Import(context.Background(), d, minimap.Options{Terrain: ter, Maps: []string{"other"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Terrain) != 0 || len(rep.Maps) != 0 || rep.TerrainApply.Accepted != 0 {
		t.Fatalf("filtered import wrote data: %+v", rep)
	}
}

func TestImportReportsCorruptFiles(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	// Corrupt one change file and one window file.
	for _, p := range []string{
		exp.Root + "/gw-test~2E1/nauvis/player/0/-1_0/1791630005000.c.gz",
		exp.Root + "/gw-test~2E1/nauvis/m/1791630300001.m.gz",
	} {
		if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := minimap.Open(exp.Root)
	if err != nil {
		t.Fatal(err)
	}
	s := stores{memstore.NewTerrain(), memstore.NewMovers(), memstore.NewOverlays()}
	rep, err := minimap.Import(context.Background(), d, minimap.Options{Terrain: s.t, Movers: s.m, Overlays: s.o})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Terrain[0].Errors) != 1 || len(rep.Movers[0].Errors) != 1 || rep.Terrain[0].ErrorCount != 1 ||
		rep.Movers[0].ErrorCount != 1 || rep.FileErrors() != 2 {
		t.Fatalf("errors not reported: terrain %v movers %v", rep.Terrain[0].Errors, rep.Movers[0].Errors)
	}
	tv, err := minimap.VerifyTerrain(context.Background(), d, s.t, minimap.Options{}, minimap.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tv.OK() {
		t.Fatal("verify passed with a corrupt change file")
	}
}

// shiftingMovers moves one sample by a tile on its way into the store, keeping every
// count the same.
type shiftingMovers struct {
	*memstore.Movers
	id string
	t  core.TimeMs
}

func (s shiftingMovers) Append(ctx context.Context, samples []core.MoverSample) (core.ApplyResult, error) {
	for i := range samples {
		if samples[i].Meta.ID == s.id && samples[i].T == s.t {
			samples[i].X++
		}
	}
	return s.Movers.Append(ctx, samples)
}

func TestVerifyMoversDetectsDifferences(t *testing.T) {
	ctx := context.Background()
	exp := minimaptest.Write(t, t.TempDir())
	d, err := minimap.Open(exp.Root)
	if err != nil {
		t.Fatal(err)
	}
	store := shiftingMovers{memstore.NewMovers(), exp.Vehicle, core.TimeMs(minimaptest.T0 + 2000)}
	rep, err := minimap.Import(ctx, d, minimap.Options{Movers: store})
	if err != nil {
		t.Fatal(err)
	}
	mv, err := minimap.VerifyMovers(ctx, d, rep, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(mv) != 1 || mv[0].Match || !mv[0].CountsMatch || mv[0].Missing != 1 || mv[0].Extra != 2 || len(mv[0].Examples) != 1 {
		t.Fatalf("verify should find the moved sample: %+v", mv)
	}
}
