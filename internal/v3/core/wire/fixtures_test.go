package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

// The web fixtures (web/fixtures) are generated from the reference stores and codecs:
//
//	go test ./internal/v3/core/wire -run TestFixtures -update
//
// Without -update the test regenerates them in memory and fails if the committed files
// differ, so fixtures cannot drift from the Go contract.
var updateFixtures = flag.Bool("update", false, "rewrite web/fixtures")

const fixtureDir = "../../../../web/fixtures"

// FixtureT0 is the fixtures' base time: 2026-10-10T10:00:00Z, hour aligned.
const FixtureT0 core.TimeMs = 1_791_630_000_000

var (
	fixDS  = core.DatastoreKey{Map: "gw-demo.1", Surface: "nauvis", Force: "player"}
	fixSfc = fixDS.SurfaceKey()

	colGrass    = core.RGB565(80, 100, 40)
	colWater    = core.RGB565(40, 80, 140)
	colRail     = core.RGB565(140, 142, 140)
	colEnemy    = core.RGB565(255, 24, 25)
	colConcrete = core.RGB565(100, 100, 100)
)

type fixtures struct {
	files    map[string][]byte
	expected map[string]any
}

func (f *fixtures) put(name string, b []byte)  { f.files[name] = b }
func (f *fixtures) expect(name string, v any)  { f.expected[name] = v }
func (f *fixtures) putJSON(name string, v any) { f.put(name, mustJSON(v)) }
func fill(v uint16) *core.ChunkPixels {
	var p core.ChunkPixels
	for i := range p {
		p[i] = v
	}
	return &p
}
func ptr[T any](v T) *T { return &v }
func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// tilePixel returns the tile pixel coordinates of pixel i of chunk c in z0 tile (0,0).
func probe(im *core.TileImage, px, py int) []int {
	return []int{px, py, int(im.Pix[py*core.TileSize+px])}
}

func buildFixtures(t *testing.T) *fixtures {
	ctx := context.Background()
	f := &fixtures{files: map[string][]byte{}, expected: map[string]any{}}
	ter := memstore.NewTerrain()
	pyr := memstore.NewPyramid(ter)
	T0 := FixtureT0

	// Terrain: four rows of grass chunks with a rail line, a water chunk, and one chunk of
	// concrete in the neighbouring tile.
	var ups []core.ChunkUpdate
	for cy := int32(-8); cy <= -5; cy++ {
		for cx := int32(-8); cx <= 7; cx++ {
			px := fill(colGrass)
			if cy == -6 {
				for x := 0; x < core.ChunkSize; x++ {
					px[16*core.ChunkSize+x] = colRail
				}
			}
			ups = append(ups, core.ChunkUpdate{DS: fixDS, Chunk: core.ChunkCoord{X: cx, Y: cy}, T: T0 + 1000, Pixels: px})
		}
	}
	ups = append(ups,
		core.ChunkUpdate{DS: fixDS, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: T0 + 2000, Pixels: fill(colWater)},
		core.ChunkUpdate{DS: fixDS, Chunk: core.ChunkCoord{X: 8, Y: -8}, T: T0 + 3000, Pixels: fill(colConcrete)},
	)
	get(ter.Apply(ctx, ups))(t)
	must0(t, ter.Commit(ctx, T0+core.Minute))
	enemy := fill(colGrass)
	for i := 0; i < 10; i++ {
		enemy[100+i] = colEnemy
	}
	get(ter.Apply(ctx, []core.ChunkUpdate{
		{DS: fixDS, Chunk: core.ChunkCoord{X: 2, Y: -7}, T: T0 + 61_000, Pixels: enemy},
		{DS: fixDS, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: T0 + 62_000, Deleted: true},
	}))(t)
	must0(t, ter.Commit(ctx, T0+2*core.Minute))
	get(ter.Apply(ctx, []core.ChunkUpdate{{DS: fixDS, Chunk: core.ChunkCoord{X: 3, Y: -7}, T: T0 + 125_000, Pixels: fill(colWater)}}))(t)

	pal := get(ter.Palette(ctx, fixDS))(t)
	lut := NewLUT(pal)
	f.put("palette.bin", EncodePalette(pal))
	f.expect("palette.bin", map[string]any{"count": len(pal.Colors), "colors": pal.Colors})

	tileFile := func(name string, tile core.TileCoord, v core.Version) {
		im := get(pyr.ReadVersion(ctx, fixDS, tile, v))(t)
		var l LUT
		if tile.Z == 0 {
			l = lut
		}
		b := get(EncodeTile(TileHeader{Z: tile.Z, X: tile.X, Y: tile.Y, Version: v}, im, l))(t)
		h := get(DecodeTileHeader(b))(t)
		f.put(name, b)
		f.expect(name, tileExpect(h, im))
	}
	t00 := core.TileCoord{Z: 0, X: 0, Y: 0}
	tileFile("tile_z0_0_0_v1.bin", t00, 1)
	tileFile("tile_z0_0_0_v2.bin", t00, 2)
	tileFile("tile_z0_1_0_v1.bin", core.TileCoord{Z: 0, X: 1, Y: 0}, 1)
	parent := core.TileCoord{Z: -1, X: 0, Y: 0}
	pst := get(pyr.VersionAt(ctx, fixDS, parent, math.MaxInt64))(t)
	tileFile(fmt.Sprintf("tile_zm1_0_0_v%d.bin", pst.Version), parent, pst.Version)

	liveIm, liveSt, err := ter.ReadTile(ctx, fixDS, t00, T0+125_000, true)
	if err != nil {
		t.Fatal(err)
	}
	b := get(EncodeTile(TileHeader{Z: 0, Version: liveSt.Version, Live: true}, liveIm, lut))(t)
	f.put("tile_z0_0_0_live.bin", b)
	f.expect("tile_z0_0_0_live.bin", tileExpect(get(DecodeTileHeader(b))(t), liveIm))

	deltas := get(ter.ChangedChunks(ctx, fixDS, t00, 1, 2))(t)
	db := get(EncodeTileDelta(DeltaHeader{X: 0, Y: 0, From: 1, To: 2}, deltas, lut))(t)
	dh, _, err := DecodeTileDelta(db, &pal)
	if err != nil {
		t.Fatal(err)
	}
	f.put("tiledelta_0_0_1-2.bin", db)
	blocks := []int{}
	for _, d := range deltas {
		blocks = append(blocks, int(d.Block))
	}
	f.expect("tiledelta_0_0_1-2.bin", map[string]any{
		"format": dh.Format, "x": dh.X, "y": dh.Y, "from": dh.From, "to": dh.To,
		"palette_version": dh.PaletteVersion, "count": dh.Count, "blocks": blocks,
		// [entry, pixel index within the chunk, rgb565]
		"probes": [][]int{{0, 100, int(deltas[0].Pixels[100])}, {0, 0, int(deltas[0].Pixels[0])}, {1, 5, int(deltas[1].Pixels[5])}},
	})

	// View and timeline.
	now := T0 + 125_000
	view := core.ViewResponse{DS: fixDS.ID(), Z: 0, T: now, Tiles: []core.ViewTile{}}
	timeline := core.TimelineResponse{DS: fixDS.ID(), Z: 0, Date: core.UTCDay(T0), Tiles: []core.TimelineTile{}}
	dayStart := get(core.ParseUTCDay(timeline.Date))(t)
	timeline.Start = dayStart
	dss := get(ter.Datastores(ctx))(t)
	view.Horizon = dss[0].Horizon
	for _, ti := range get(ter.ListTiles(ctx, fixDS, 0))(t) {
		st := get(ter.VersionAt(ctx, fixDS, ti.Tile, now))(t)
		view.Tiles = append(view.Tiles, core.ViewTile{X: ti.Tile.X, Y: ti.Tile.Y, TileState: st})
		tt := core.TimelineTile{X: ti.Tile.X, Y: ti.Tile.Y, Points: [][2]int64{}}
		tt.V0 = get(ter.VersionAt(ctx, fixDS, ti.Tile, dayStart-1))(t).Version
		for _, p := range get(ter.History(ctx, fixDS, ti.Tile, dayStart, dayStart+core.Day))(t) {
			tt.Points = append(tt.Points, [2]int64{int64(p.T), int64(p.Version)})
		}
		timeline.Tiles = append(timeline.Tiles, tt)
	}
	f.putJSON("view_z0.json", view)
	f.putJSON("timeline_z0.json", timeline)

	// Movers.
	mv := memstore.NewMovers()
	samples := moverSamples(T0)
	get(mv.Append(ctx, samples))(t)
	must0(t, mv.Commit(ctx, T0+core.Hour))
	get(mv.Append(ctx, []core.MoverSample{{Surface: fixSfc, Meta: playerMeta, T: T0 + core.Hour + 5000, X: 12, Y: 14, Orientation: 0.5, Flags: core.FlagNoTick}}))(t)
	moverFile := func(name string, w *core.MoverWindow) {
		b := get(EncodeMoverWindow(w))(t)
		f.put(name, b)
		f.expect(name, windowExpect(w))
	}
	moverFile("movers_window.bin", get(mv.Window(ctx, fixSfc, T0))(t))
	moverFile("movers_keyframe.bin", get(mv.Keyframe(ctx, fixSfc, T0+core.Hour))(t))
	moverFile("movers_live.bin", get(mv.Live(ctx, fixSfc, T0+core.Hour+10_000))(t))
	f.putJSON("movers_day.json", get(mv.Day(ctx, fixSfc, core.UTCDay(T0)))(t))

	// Overlays.
	ov := memstore.NewOverlays()
	get(ov.Apply(ctx, []core.OverlayEvent{
		{DS: fixDS, T: T0 + 1000, Interval: core.Interval{Kind: core.OverlayTag, Key: "248495586/1", Instance: 248495586, X: -100, Y: -150, Icon: "item/iron-ore", Text: "21M"}},
		{DS: fixDS, T: T0 + 30_000, Interval: core.Interval{Kind: core.OverlayTag, Key: "248495586/1", Instance: 248495586, X: -100, Y: -150, Icon: "item/iron-ore", Text: "12M"}},
		{DS: fixDS, T: T0 + 2000, Interval: core.Interval{Kind: core.OverlayRecipe, Key: "248495586/5001", Instance: 248495586, X: -60.5, Y: -140.5, Recipe: "iron-gear-wheel", Quality: "normal"}},
		{DS: fixDS, T: T0 + 2000, Interval: core.Interval{Kind: core.OverlayRail, Key: "248495586/7001", Instance: 248495586, X: -99, Y: -175, Name: "straight-rail", Direction: 4}},
	}))(t)
	ivs := get(ov.Query(ctx, fixDS, core.OverlayQuery{Tile: t00, T0: 0, T1: math.MaxInt64}))(t)
	f.putJSON("overlays_0_0.json", core.OverlayResponse{DS: fixDS.ID(), X: 0, Y: 0, Version: get(ov.Version(ctx, fixDS, t00))(t), Intervals: ivs})

	// Catalog.
	sfcs := get(mv.Surfaces(ctx))(t)
	d := dss[0]
	cat := core.Catalog{
		Version: core.APIVersion,
		Now:     T0 + core.Hour + 10_000,
		LiveMs:  5000,
		Maps: []core.CatalogMap{{
			MapInfo: core.MapInfo{
				Key: fixDS.Map, Title: "Demo gridworld", TileSize: 512,
				Instances: []core.InstanceInfo{
					{ID: "248495586", Name: "gridworld_0_0", Bounds: &core.Quad{-256, -256, 256, 256}},
					{ID: "1151964797", Name: "gridworld_1_0", Bounds: &core.Quad{256, -256, 768, 256}},
				},
			},
			Surfaces: []core.CatalogSurface{{
				Name:  fixDS.Surface,
				ID:    fixSfc.ID(),
				Edges: []core.Quad{{256, -256, 256, 256}},
				Datastores: []core.CatalogDatastore{{
					Force: fixDS.Force, ID: d.ID, WindowMs: d.Window, Horizon: d.Horizon, First: d.FirstT, Last: d.LastT,
					PaletteVersion: d.PaletteVersion, Bounds: d.Bounds, Levels: []int8{0, -1},
				}},
				Movers: &core.CatalogMovers{Horizon: sfcs[0].Horizon, First: sfcs[0].FirstT, Last: sfcs[0].LastT, Days: sfcs[0].Days},
			}},
		}},
	}
	f.putJSON("catalog.json", cat)

	// Capture envelopes.
	f.put("envelopes.ndjson", envelopes(T0))

	f.putJSON("expected.json", f.expected)
	return f
}

func tileExpect(h TileHeader, im *core.TileImage) map[string]any {
	// Probes: [px, py, rgb565]. Rail row (world y -176), water chunk (0,0) at tile pixel
	// (256,256), the enemy pixels of chunk (2,-7), the live chunk (3,-7), and a corner.
	return map[string]any{
		"format": h.Format, "z": h.Z, "x": h.X, "y": h.Y, "version": h.Version, "live": h.Live,
		"palette_version": h.PaletteVersion,
		"probes": [][]int{
			probe(im, 0, 0), probe(im, 5, 80), probe(im, 256, 256), probe(im, 10*32+4, 32+3),
			probe(im, 11*32+1, 32+1), probe(im, 511, 511), probe(im, 128, 40),
		},
	}
}

func windowExpect(w *core.MoverWindow) map[string]any {
	ids := []string{}
	for _, m := range w.Movers {
		ids = append(ids, m.ID)
	}
	row := func(r core.MoverRow) map[string]any {
		return map[string]any{"mover": r.Mover, "t": r.T, "tick": r.Tick, "x": r.X, "y": r.Y, "o": r.Orientation, "flags": r.Flags, "carriages": r.Carriages}
	}
	out := map[string]any{"kind": w.Kind, "start": w.Start, "len": w.Len, "movers": ids, "rows": len(w.Rows)}
	if len(w.Rows) > 0 {
		out["first"] = row(w.Rows[0])
		out["last"] = row(w.Rows[len(w.Rows)-1])
		var flagged []map[string]any
		for _, r := range w.Rows {
			if r.Flags != 0 || len(r.Carriages) > 0 {
				flagged = append(flagged, row(r))
			}
		}
		out["flagged"] = flagged
	}
	return out
}

var (
	trainMeta  = core.MoverMeta{ID: "248495586.1000.7", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3, Kinds: "LCC", Color: "f20000"}
	spiderMeta = core.MoverMeta{ID: "248495586.1200.v55", Kind: core.KindVehicle, Force: "player", Name: "spidertron", Kinds: "S", Color: "00ff00"}
	playerMeta = core.MoverMeta{ID: "p:alice", Kind: core.KindPlayer, Force: "player", Name: "alice"}
)

// moverSamples builds a train that waits, runs east along the rail at y=-176, turns
// south on a radius-32 curve, crosses an instance border (gone + resume = handoff) and
// stops; a spidertron; and a player from imported data (no ticks).
func moverSamples(T0 core.TimeMs) []core.MoverSample {
	const baseTick = 5_000_000
	var out []core.MoverSample
	add := func(meta core.MoverMeta, t core.TimeMs, x, y, o float64, flags core.MoverFlags, cars []core.Carriage) {
		s := core.MoverSample{Surface: fixSfc, Meta: meta, T: t, X: x, Y: y, Orientation: o, Flags: flags, Carriages: cars}
		if flags&core.FlagNoTick == 0 {
			s.Tick = baseTick + int64(t-T0)*60/1000
		}
		out = append(out, s)
	}
	carsEast := func(x, y float64) []core.Carriage {
		return []core.Carriage{{X: x, Y: y, Orientation: 0.25}, {X: x - 7, Y: y, Orientation: 0.25}, {X: x - 14, Y: y, Orientation: 0.25}}
	}
	t := T0 + 500
	add(trainMeta, t, -100, -176, 0.25, core.FlagStop, carsEast(-100, -176))
	x := -100.0
	for x < -32 { // east at 40 tiles/s, a sample every 250 ms
		t += 250
		x = math.Min(x+10, -32)
		add(trainMeta, t, x, -176, 0.25, 0, nil)
	}
	for i := 1; i <= 5; i++ { // quarter circle, centre (-32, -144), radius 32
		th := float64(i) / 5 * math.Pi / 2
		t += 250
		add(trainMeta, t, -32+32*math.Sin(th), -144-32*math.Cos(th), 0.25+th/(2*math.Pi), 0, nil)
	}
	y := -144.0
	for i := 0; i < 4; i++ {
		t += 250
		y += 10
		add(trainMeta, t, 0, y, 0.5, 0, nil)
	}
	t += 300
	add(trainMeta, t, 0, y, 0.5, core.FlagGone, nil) // crossing an instance border
	t += 600
	y += 34
	add(trainMeta, t, 0, y, 0.5, 0, nil)
	t += 500
	y += 10
	add(trainMeta, t, 0, y, 0.5, core.FlagStop, []core.Carriage{{X: 0, Y: y, Orientation: 0.5}, {X: 0, Y: y - 7, Orientation: 0.5}, {X: 0, Y: y - 14, Orientation: 0.5}})
	for i := 0; i < 6; i++ {
		add(spiderMeta, T0+core.TimeMs(1000+500*i), 50+float64(i)*3, 60-float64(i), 0.2+float64(i)*0.01, 0, nil)
	}
	for i := 0; i < 4; i++ {
		add(playerMeta, T0+core.TimeMs(2000+1000*i), 10+float64(i)*0.5, 12+float64(i)*0.25, 0, core.FlagNoTick|core.FlagNoHeading, nil)
	}
	return out
}

func envelopes(T0 core.TimeMs) []byte {
	px := fill(colGrass)
	px[0] = colRail
	d := core.EncodeChart(px)
	msg := func(v any) json.RawMessage { return mustJSONCompact(v) }
	env := []core.Envelope{
		{
			V: core.ProtocolVersion, Tick: 5_000_000,
			Src: "clusterio:248495586", Map: fixDS.Map, Instance: 248495586, RecvMs: T0 + 1000, Seq: ptr(int64(1)),
			Msgs: []json.RawMessage{
				msg(core.SurfacesMsg{Type: core.MsgSurfaces, List: []core.SurfaceEntry{{Name: "nauvis", Tile: true}}}),
				msg(core.ChunkMsg{Type: core.MsgChunk, S: "nauvis", F: "player", X: 0, Y: -6, D: &d}),
				msg(core.ChunkMsg{Type: core.MsgChunk, S: "nauvis", F: "player", X: 1, Y: -6, Tick: ptr(int64(4_999_990))}),
				msg(core.MoveMsg{Type: core.MsgMove, S: "nauvis", M: []core.MoveEntry{
					{J: trainMeta.ID, K: "t", F: "player", Name: "locomotive", X: -100, Y: -176, H: ptr(0.25), N: 3, O: "LCC", C: "f20000", Stop: true,
						Car: [][3]float64{{-100, -176, 0.25}, {-107, -176, 0.25}, {-114, -176, 0.25}}},
					{J: spiderMeta.ID, K: "v", F: "player", Name: "spidertron", X: 50, Y: 60, H: ptr(0.2), O: "S", C: "00ff00", Tick: ptr(int64(4_999_995))},
					{J: "p:alice", K: "p", F: "player", Name: "alice", X: 10, Y: 12},
				}}),
				msg(core.TagMsg{Type: core.MsgTag, S: "nauvis", F: "player", N: 1, X: ptr(-100.0), Y: ptr(-150.0), Icon: "item/iron-ore", Text: "21M"}),
				msg(core.RecipeMsg{Type: core.MsgRecipe, S: "nauvis", F: "player", U: 5001, X: ptr(-60.5), Y: ptr(-140.5), R: "iron-gear-wheel", Q: "normal"}),
				msg(core.RailMsg{Type: core.MsgRail, S: "nauvis", F: "player", U: 7001, Name: "straight-rail", X: ptr(-99.0), Y: ptr(-175.0), Dir: 4}),
			},
		},
		{
			V: core.ProtocolVersion, Tick: 5_000_060,
			Src: "clusterio:248495586", Map: fixDS.Map, Instance: 248495586, RecvMs: T0 + 2003, Seq: ptr(int64(2)),
			Msgs: []json.RawMessage{
				msg(core.MoveMsg{Type: core.MsgMove, S: "nauvis", M: []core.MoveEntry{{J: trainMeta.ID, Gone: true}}}),
				msg(core.TagMsg{Type: core.MsgTag, S: "nauvis", F: "player", N: 1}),
				msg(core.RecipeMsg{Type: core.MsgRecipe, S: "nauvis", F: "player", U: 5001}),
				msg(core.AdoptMsg{Type: core.MsgAdopt, J: trainMeta.ID, Trace: "ue-17"}),
				msg(core.ResyncedMsg{Type: core.MsgResynced, Trace: "start-1", Counts: map[string]int64{"chunks": 64, "drops": 0}}),
				msg(core.TraceMsg{Type: core.MsgTrace, Op: "resync", Phase: "done", Level: "info"}),
			},
		},
		{
			V: core.ProtocolVersion, Tick: 0,
			Src: "clusterio:controller", Map: fixDS.Map, RecvMs: T0 + 2500,
			Msgs: []json.RawMessage{msg(core.MapMsg{Type: core.MsgMap, Info: core.MapInfo{Key: fixDS.Map, Title: "Demo gridworld", TileSize: 512,
				Instances: []core.InstanceInfo{{ID: "248495586", Name: "gridworld_0_0", Bounds: &core.Quad{-256, -256, 256, 256}}},
				Edges:     map[string][]core.Quad{"nauvis": {{256, -256, 256, 256}}}}})},
		},
	}
	var buf bytes.Buffer
	for _, e := range env {
		buf.Write(mustJSONCompact(e))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func mustJSONCompact(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func get[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func must0(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixtures(t *testing.T) {
	f := buildFixtures(t)
	names := make([]string, 0, len(f.files))
	for n := range f.files {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateFixtures {
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(fixtureDir, n), f.files[n], 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %d fixtures to %s", len(names), fixtureDir)
		return
	}
	for _, n := range names {
		got, err := os.ReadFile(filepath.Join(fixtureDir, n))
		if err != nil {
			t.Fatalf("%s: %v (run go test ./internal/v3/core/wire -run TestFixtures -update)", n, err)
		}
		got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")) // tolerate CRLF checkouts of text files
		if !bytes.Equal(got, f.files[n]) {
			t.Fatalf("%s differs from the generated fixture (run with -update and review the diff)", n)
		}
	}
}

// TestFixturesDecode checks that every generated binary fixture decodes.
func TestFixturesDecode(t *testing.T) {
	f := buildFixtures(t)
	pal, err := DecodePalette(f.files["palette.bin"])
	if err != nil {
		t.Fatal(err)
	}
	for n, b := range f.files {
		switch {
		case len(n) > 5 && n[:5] == "tile_":
			if _, _, err := DecodeTile(b, &pal); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
		case len(n) > 10 && n[:10] == "tiledelta_":
			if _, _, err := DecodeTileDelta(b, &pal); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
		case len(n) > 7 && n[:7] == "movers_" && filepath.Ext(n) == ".bin":
			if _, err := DecodeMoverWindow(b, fixSfc); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
		}
	}
	for _, line := range bytes.Split(bytes.TrimSpace(f.files["envelopes.ndjson"]), []byte("\n")) {
		var e core.Envelope
		if err := json.Unmarshal(line, &e); err != nil || e.V != core.ProtocolVersion {
			t.Fatalf("envelope %s: %v", line, err)
		}
	}
}
