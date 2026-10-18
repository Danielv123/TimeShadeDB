// Package coretest holds contract tests for the core store interfaces. Every store
// implementation runs them from its own tests:
//
//	func TestContract(t *testing.T) {
//		coretest.TerrainStore(t, func(t *testing.T) core.TerrainStore { return terrain.OpenTemp(t) })
//		coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore { ... }, coretest.DiffOptions{})
//	}
//
// The reference implementations in core/memstore pass all of them. Expectations are the
// normative semantics written in the core interface comments.
package coretest

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

// TerrainFactory opens a new, empty store. The test closes it.
type TerrainFactory func(t *testing.T) core.TerrainStore

// DS is the datastore key the contract tests use.
var DS = core.DatastoreKey{Map: "gw-test.1", Surface: "nauvis", Force: "player"}

// Fill returns a chunk image with every pixel set to v.
func Fill(v uint16) *core.ChunkPixels {
	var px core.ChunkPixels
	for i := range px {
		px[i] = v
	}
	return &px
}

// get turns a (value, error) result into a function that fails the test on error:
// v := get(store.Method(...))(t).
func get[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func blockOf(t *testing.T, s core.TerrainStore, tile core.TileCoord, v core.Version, block uint8) core.ChunkPixels {
	t.Helper()
	im := get(s.ReadVersion(context.Background(), DS, tile, v))(t)
	return im.Block(block)
}

// TerrainStore runs the terrain contract tests.
func TerrainStore(t *testing.T, open TerrainFactory) {
	ctx := context.Background()
	const W = core.DefaultTerrainWindow
	c0 := core.ChunkCoord{X: 0, Y: 0}
	tile, block := c0.Tile() // tile 0/0/0, block 136

	t.Run("ApplyCommitRead", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		res := get(s.Apply(ctx, []core.ChunkUpdate{{DS: DS, Chunk: c0, T: 1000, Pixels: Fill(5)}}))(t)
		if res != (core.ApplyResult{Accepted: 1}) {
			t.Fatalf("apply result = %+v", res)
		}
		st := get(s.VersionAt(ctx, DS, tile, 1000))(t)
		if st.Version != 0 || !st.Live || st.LiveSeq == 0 {
			t.Fatalf("state before commit = %+v, want v0 live", st)
		}
		im, st2, err := s.ReadTile(ctx, DS, tile, 1000, true)
		if err != nil || im.Block(block) != *Fill(5) || !st2.Live {
			t.Fatalf("live read = %v %+v, want block of 5", err, st2)
		}
		im, st2, err = s.ReadTile(ctx, DS, tile, 1000, false)
		if err != nil || !im.IsEmpty() || st2.Live {
			t.Fatalf("committed read before commit not empty (%v, %+v)", err, st2)
		}
		if st := get(s.VersionAt(ctx, DS, tile, 999))(t); st.Live {
			t.Fatalf("live offered before the newest update time: %+v", st)
		}
		infos := get(s.ListTiles(ctx, DS, 0))(t)
		if len(infos) != 1 || infos[0].Tile != tile || infos[0].Version != 0 || !infos[0].Live {
			t.Fatalf("ListTiles before commit = %+v", infos)
		}
		if err := s.Commit(ctx, W); err != nil {
			t.Fatal(err)
		}
		if st := get(s.VersionAt(ctx, DS, tile, W-1))(t); st.Version != 0 || st.Live {
			t.Fatalf("state just before window end = %+v", st)
		}
		if st := get(s.VersionAt(ctx, DS, tile, W))(t); st.Version != 1 || st.Live {
			t.Fatalf("state at window end = %+v", st)
		}
		if got := blockOf(t, s, tile, 1, block); got != *Fill(5) {
			t.Fatal("version 1 content wrong")
		}
		if got := get(s.ReadVersion(ctx, DS, tile, 0))(t); !got.IsEmpty() {
			t.Fatal("version 0 not empty")
		}
		hist := get(s.History(ctx, DS, tile, 0, 10*W))(t)
		if len(hist) != 1 || hist[0] != (core.VersionPoint{T: W, Version: 1}) {
			t.Fatalf("history = %+v", hist)
		}
		infos = get(s.ListTiles(ctx, DS, 0))(t)
		if len(infos) != 1 || infos[0].Version != 1 || infos[0].FirstT != W || infos[0].LastT != W || infos[0].Live {
			t.Fatalf("ListTiles after commit = %+v", infos)
		}
		dss := get(s.Datastores(ctx))(t)
		if len(dss) != 1 {
			t.Fatalf("datastores = %+v", dss)
		}
		d := dss[0]
		if d.Key != DS || d.ID != DS.ID() || d.Window != W || d.Horizon != W || d.FirstT != 1000 || d.LastT != 1000 ||
			d.Tiles != 1 || d.Bounds != [4]int32{0, 0, 0, 0} || d.PaletteVersion != 2 {
			t.Fatalf("datastore info = %+v", d)
		}
	})

	t.Run("LateAndHorizon", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		get(s.Apply(ctx, []core.ChunkUpdate{{DS: DS, Chunk: c0, T: 1000, Pixels: Fill(5)}}))(t)
		if err := s.Commit(ctx, W+W/2); err != nil {
			t.Fatal(err)
		}
		res := get(s.Apply(ctx, []core.ChunkUpdate{
			{DS: DS, Chunk: c0, T: W - 1, Pixels: Fill(6)},
			{DS: DS, Chunk: c0, T: W, Pixels: Fill(7)},
		}))(t)
		if res != (core.ApplyResult{Accepted: 1, Late: 1}) {
			t.Fatalf("apply result = %+v", res)
		}
		// Commit never moves the horizon backwards.
		if err := s.Commit(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if d := get(s.Datastores(ctx))(t)[0]; d.Horizon != W {
			t.Fatalf("horizon = %d, want %d", d.Horizon, W)
		}
		if err := s.Commit(ctx, 2*W); err != nil {
			t.Fatal(err)
		}
		if got := blockOf(t, s, tile, 2, block); got != *Fill(7) {
			t.Fatal("version 2 content wrong")
		}
	})

	t.Run("NetOutAndLatestWins", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		get(s.Apply(ctx, []core.ChunkUpdate{{DS: DS, Chunk: c0, T: 1000, Pixels: Fill(5)}}))(t)
		ok(t, s.Commit(ctx, W))
		// A -> B -> A inside one window: no new version.
		get(s.Apply(ctx, []core.ChunkUpdate{
			{DS: DS, Chunk: c0, T: W + 1000, Pixels: Fill(6)},
			{DS: DS, Chunk: c0, T: W + 3000, Pixels: Fill(5)},
			{DS: DS, Chunk: c0, T: W + 2000, Pixels: Fill(9)}, // older than the newest: loses
		}))(t)
		ok(t, s.Commit(ctx, 2*W))
		if st := get(s.VersionAt(ctx, DS, tile, 5*W))(t); st.Version != 1 {
			t.Fatalf("net-out window created a version: %+v", st)
		}
		// Equal T: the later update wins.
		get(s.Apply(ctx, []core.ChunkUpdate{
			{DS: DS, Chunk: c0, T: 2*W + 10, Pixels: Fill(1)},
			{DS: DS, Chunk: c0, T: 2*W + 10, Pixels: Fill(2)},
		}))(t)
		ok(t, s.Commit(ctx, 3*W))
		if got := blockOf(t, s, tile, 2, block); got != *Fill(2) {
			t.Fatal("equal-T tie not won by the later update")
		}
	})

	t.Run("DeletedAndDeltas", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		c1 := core.ChunkCoord{X: 1, Y: 0} // block 137
		_, b1 := c1.Tile()
		get(s.Apply(ctx, []core.ChunkUpdate{
			{DS: DS, Chunk: c0, T: 10, Pixels: Fill(5)},
			{DS: DS, Chunk: c1, T: 20, Pixels: Fill(6)},
		}))(t)
		ok(t, s.Commit(ctx, W))
		get(s.Apply(ctx, []core.ChunkUpdate{{DS: DS, Chunk: c1, T: W + 5, Deleted: true}}))(t)
		ok(t, s.Commit(ctx, 2*W))
		if got := blockOf(t, s, tile, 2, b1); got != (core.ChunkPixels{}) {
			t.Fatal("deleted chunk not uncharted")
		}
		deltas := get(s.ChangedChunks(ctx, DS, tile, 1, 2))(t)
		if len(deltas) != 1 || deltas[0].Block != b1 || deltas[0].Pixels != (core.ChunkPixels{}) {
			t.Fatalf("deltas 1-2 = %+v", deltas)
		}
		deltas = get(s.ChangedChunks(ctx, DS, tile, 0, 1))(t)
		if len(deltas) != 2 || deltas[0].Block != block || deltas[1].Block != b1 {
			t.Fatalf("deltas 0-1 = %d entries", len(deltas))
		}
		// Chunk 1 is back to its version-0 content at version 2, so 0-2 holds only chunk 0.
		deltas = get(s.ChangedChunks(ctx, DS, tile, 0, 2))(t)
		if len(deltas) != 1 || deltas[0].Block != block {
			t.Fatalf("deltas 0-2 = %+v", deltas)
		}
		if _, err := s.ChangedChunks(ctx, DS, tile, 2, 2); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("empty delta range err = %v", err)
		}
	})

	t.Run("Errors", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		res := get(s.Apply(ctx, []core.ChunkUpdate{
			{DS: core.DatastoreKey{Map: "m"}, Chunk: c0, T: 1, Pixels: Fill(1)},
			{DS: DS, Chunk: c0, T: 1},
			{DS: DS, Chunk: core.ChunkCoord{X: 1 << 30, Y: 0}, T: 1, Pixels: Fill(1)},
			{DS: DS, Chunk: c0, T: 1, Pixels: Fill(1)},
		}))(t)
		if res != (core.ApplyResult{Accepted: 1, Invalid: 3}) {
			t.Fatalf("apply result = %+v", res)
		}
		ok(t, s.Commit(ctx, W))
		other := core.DatastoreKey{Map: "nope", Surface: "nope", Force: "nope"}
		checks := []struct {
			name string
			err  error
			want error
		}{
			{"bad level", second(s.VersionAt(ctx, DS, core.TileCoord{Z: 1}, W)), core.ErrBadTile},
			{"coarse level", second(s.VersionAt(ctx, DS, core.TileCoord{Z: -1}, W)), core.ErrBadTile},
			{"out of grid", second(s.VersionAt(ctx, DS, core.TileCoord{X: core.MaxCoord + 1}, W)), core.ErrBadTile},
			{"unknown ds", second(s.VersionAt(ctx, other, tile, W)), core.ErrUnknownDatastore},
			{"no tile", second(s.VersionAt(ctx, DS, core.TileCoord{X: 5, Y: 5}, W)), core.ErrNoTile},
			{"no version", second(s.ReadVersion(ctx, DS, tile, 2)), core.ErrNoVersion},
			{"list coarse", second(s.ListTiles(ctx, DS, -1)), core.ErrBadTile},
			{"palette unknown", second(s.Palette(ctx, other)), core.ErrUnknownDatastore},
		}
		for _, c := range checks {
			if !errors.Is(c.err, c.want) {
				t.Fatalf("%s: err = %v, want %v", c.name, c.err, c.want)
			}
		}
	})

	t.Run("PaletteAndWindow", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		if err := s.EnsureDatastore(ctx, DS, 10*core.Second); err != nil {
			t.Fatal(err)
		}
		px := Fill(0xF8C3)
		px[0] = 0x1234
		get(s.Apply(ctx, []core.ChunkUpdate{{DS: DS, Chunk: c0, T: 15_000, Pixels: px}}))(t)
		ok(t, s.Commit(ctx, 20_000))
		pal := get(s.Palette(ctx, DS))(t)
		if len(pal.Colors) != 3 || pal.Colors[0] != 0 || pal.Version() != 3 {
			t.Fatalf("palette = %v", pal.Colors)
		}
		seen := map[uint16]bool{}
		for _, c := range pal.Colors {
			seen[c] = true
		}
		if !seen[0xF8C3] || !seen[0x1234] {
			t.Fatalf("palette misses colours: %v", pal.Colors)
		}
		if st := get(s.VersionAt(ctx, DS, tile, 20_000))(t); st.Version != 1 {
			t.Fatalf("10 s window not honoured: %+v", st)
		}
		if d := get(s.Datastores(ctx))(t)[0]; d.Window != 10*core.Second || d.Horizon != 20_000 {
			t.Fatalf("datastore = %+v", d)
		}
	})
}

func second[T any](_ T, err error) error { return err }

// DiffOptions tunes TerrainDifferential.
type DiffOptions struct {
	// Seed for the random stream (default 1).
	Seed int64
	// Updates is the number of updates (default 3000).
	Updates int
	// ExactVersions also compares version numbers, which only holds for stores that do
	// not flush windows early.
	ExactVersions bool
}

// TerrainDifferential feeds the same random update stream (several datastores, tiles,
// flicker, deletes, late and out-of-order updates, periodic commits) to the store under
// test and to memstore.Terrain, and compares every observable result.
func TerrainDifferential(t *testing.T, open TerrainFactory, opt DiffOptions) {
	ctx := context.Background()
	if opt.Seed == 0 {
		opt.Seed = 1
	}
	if opt.Updates == 0 {
		opt.Updates = 3000
	}
	r := rand.New(rand.NewSource(opt.Seed))
	got := open(t)
	defer got.Close()
	want := memstore.NewTerrain()
	keys := []core.DatastoreKey{DS, {Map: "gw-test.1", Surface: "vulcanus", Force: "player"}}
	colours := []uint16{0x0000, 0x8C71, 0xF8C3, 0x4208, 0x07E0, 0xFFFF}
	var now core.TimeMs = 1_791_630_000_000
	var times []core.TimeMs
	for i := 0; i < opt.Updates; i++ {
		now += core.TimeMs(r.Intn(4000))
		u := core.ChunkUpdate{
			DS:    keys[r.Intn(len(keys))],
			Chunk: core.ChunkCoord{X: int32(r.Intn(40) - 20), Y: int32(r.Intn(40) - 20)},
			T:     now - core.TimeMs(r.Intn(90_000)), // up to 1.5 windows late
		}
		if r.Intn(30) == 0 {
			u.Deleted = true
		} else {
			px := Fill(colours[r.Intn(len(colours))])
			for j := r.Intn(8); j > 0; j-- {
				px[r.Intn(core.ChunkPixelCount)] = colours[r.Intn(len(colours))]
			}
			u.Pixels = px
		}
		batch := []core.ChunkUpdate{u}
		a := get(got.Apply(ctx, batch))(t)
		b := get(want.Apply(ctx, batch))(t)
		if a != b {
			t.Fatalf("update %d: apply result %+v, oracle %+v", i, a, b)
		}
		times = append(times, u.T)
		if r.Intn(50) == 0 {
			upTo := now - core.TimeMs(r.Intn(30_000))
			ok(t, got.Commit(ctx, upTo))
			ok(t, want.Commit(ctx, upTo))
			compareTerrain(t, got, want, r, times, opt)
		}
	}
	ok(t, got.Commit(ctx, now))
	ok(t, want.Commit(ctx, now))
	compareTerrain(t, got, want, r, times, opt)
}

func compareTerrain(t *testing.T, got, want core.TerrainStore, r *rand.Rand, times []core.TimeMs, opt DiffOptions) {
	t.Helper()
	ctx := context.Background()
	gds := get(got.Datastores(ctx))(t)
	wds := get(want.Datastores(ctx))(t)
	if len(gds) != len(wds) {
		t.Fatalf("datastores: %d, oracle %d", len(gds), len(wds))
	}
	for i := range wds {
		g, w := gds[i], wds[i]
		g.PaletteVersion, w.PaletteVersion = 0, 0 // palette order and growth are store-defined
		if g != w {
			t.Fatalf("datastore info %+v, oracle %+v", g, w)
		}
		gt := get(got.ListTiles(ctx, w.Key, 0))(t)
		wt := get(want.ListTiles(ctx, w.Key, 0))(t)
		if len(gt) != len(wt) {
			t.Fatalf("%v: %d tiles, oracle %d", w.Key, len(gt), len(wt))
		}
		for j := range wt {
			if gt[j].Tile != wt[j].Tile || gt[j].Live != wt[j].Live ||
				(opt.ExactVersions && gt[j] != wt[j]) {
				t.Fatalf("%v: tile info %+v, oracle %+v", w.Key, gt[j], wt[j])
			}
		}
		for _, ti := range wt {
			for k := 0; k < 4; k++ {
				at := times[r.Intn(len(times))] + core.TimeMs(r.Intn(120_000))
				live := k%2 == 1
				gi, gs, err := got.ReadTile(ctx, w.Key, ti.Tile, at, live)
				if err != nil {
					t.Fatal(err)
				}
				wi, ws, _ := want.ReadTile(ctx, w.Key, ti.Tile, at, live)
				if !gi.Equal(wi) {
					t.Fatalf("%v %v at %d live=%v: pixels differ from oracle", w.Key, ti.Tile, at, live)
				}
				if gs.Live != ws.Live || (opt.ExactVersions && gs.Version != ws.Version) {
					t.Fatalf("%v %v at %d: state %+v, oracle %+v", w.Key, ti.Tile, at, gs, ws)
				}
			}
		}
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
