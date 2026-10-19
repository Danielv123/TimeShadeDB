package pyramid

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

var ds = coretest.DS

// t0 is an hour-aligned base time.
const t0 = core.TimeMs(497_675 * 3_600_000)

const W = core.DefaultTerrainWindow

func must[T any](t testing.TB) func(v T, err error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestDownsampleMatchesCore(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 6; iter++ {
		var kids [4]*core.TileImage
		for i := range kids {
			if iter > 0 && r.Intn(3) == 0 {
				continue
			}
			kids[i] = core.NewTileImage()
			for j := range kids[i].Pix {
				switch iter {
				case 0:
					kids[i].Pix[j] = 0xFFFF // channel maxima
				default:
					kids[i].Pix[j] = uint16(r.Intn(65536))
				}
			}
		}
		if got, want := downsample(kids), core.Downsample(kids); !got.Equal(want) {
			t.Fatalf("iteration %d: fast downsample differs from core.Downsample", iter)
		}
	}
	for i := 0; i < 2_000_000; i++ {
		a, b, c, d := uint16(r.Uint32()), uint16(r.Uint32()), uint16(r.Uint32()), uint16(r.Uint32())
		s := spread(a) + spread(b) + spread(c) + spread(d) + avgRound
		if got := uint16((s>>2)&0xF81F | (s>>18)&0x07E0); got != core.Average4(a, b, c, d) {
			t.Fatalf("average of %04x %04x %04x %04x = %04x, want %04x", a, b, c, d, got, core.Average4(a, b, c, d))
		}
	}
}

func TestDeltaCodecRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	prev := core.NewTileImage()
	for i := range prev.Pix {
		prev.Pix[i] = uint16(r.Intn(8)) * 3001
	}
	for _, perBlock := range []int{0, 1, 5, 128, 129, 600, 1023, 1024} {
		next := prev.Clone()
		for b := 0; b < core.TileBlocks; b += 1 + r.Intn(7) {
			for _, p := range r.Perm(core.ChunkPixelCount)[:perBlock] {
				i := core.TilePixelIndex(uint8(b), p)
				next.Pix[i] = prev.Pix[i] + 1
			}
		}
		raw, changed := appendDelta(nil, prev, next)
		got := prev.Clone()
		if err := applyDelta(raw, got); err != nil {
			t.Fatal(err)
		}
		if !got.Equal(next) {
			t.Fatalf("%d changes per block: round trip differs", perBlock)
		}
		if perBlock == 0 && (changed != 0 || len(raw) != 0) {
			t.Fatal("empty delta not empty")
		}
		if len(raw) > 0 {
			if err := applyDelta(raw[:len(raw)-1], prev.Clone()); err == nil {
				t.Fatal("truncated delta accepted")
			}
		}
	}
	im := core.NewTileImage()
	if err := decodeSnapshot(appendSnapshot(nil, prev), im); err != nil || !im.Equal(prev) {
		t.Fatalf("snapshot round trip: %v", err)
	}
}

// history generates chunk updates per 60 s window over a few z0 tiles (including tiles
// on both sides of the origin and one far away), with full repaints, small edits and
// deletions.
type history struct {
	windows [][]core.ChunkUpdate
}

func genHistory(seed int64, nWindows int) history {
	r := rand.New(rand.NewSource(seed))
	var chunks []core.ChunkCoord
	for i := 0; i < 40; i++ {
		chunks = append(chunks, core.ChunkCoord{X: int32(r.Intn(48) - 24), Y: int32(r.Intn(48) - 24)})
	}
	chunks = append(chunks, core.ChunkCoord{X: 300, Y: -200}, core.ChunkCoord{X: -1000, Y: 777})
	colours := []uint16{0x0000}
	for i := 0; i < 40; i++ {
		colours = append(colours, uint16(r.Intn(65535)+1))
	}
	state := map[core.ChunkCoord]core.ChunkPixels{}
	var h history
	for w := 0; w < nWindows; w++ {
		var ups []core.ChunkUpdate
		n := r.Intn(4)
		if r.Intn(5) == 0 {
			n = 0 // idle stretches
		}
		for i := 0; i < n; i++ {
			c := chunks[r.Intn(len(chunks))]
			px := state[c]
			u := core.ChunkUpdate{DS: ds, Chunk: c, T: t0 + core.TimeMs(w)*W + core.TimeMs(r.Intn(int(W)))}
			switch k := r.Intn(10); {
			case k == 0:
				u.Deleted = true
				px = core.ChunkPixels{}
			case k < 4:
				for j := range px {
					px[j] = colours[r.Intn(len(colours))]
				}
			default:
				for j := 0; j < 1+r.Intn(20); j++ {
					px[r.Intn(len(px))] = colours[r.Intn(len(colours))]
				}
			}
			state[c] = px
			if !u.Deleted {
				cp := px
				u.Pixels = &cp
			}
			ups = append(ups, u)
		}
		h.windows = append(h.windows, ups)
	}
	return h
}

// replayLive applies the history window by window, committing every window and running
// Update after each commit (a live server).
func replayLive(t testing.TB, ter *memstore.Terrain, p *Pyramid, h history, from int) {
	ctx := context.Background()
	for w := from; w < len(h.windows); w++ {
		must[core.ApplyResult](t)(ter.Apply(ctx, h.windows[w]))
		if err := ter.Commit(ctx, t0+core.TimeMs(w+1)*W); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Update(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// expectAt computes every coarse tile at time T from the z0 content with the reference
// core.Downsample, recursing only over tiles that exist.
func expectAt(t testing.TB, ter core.TerrainStore, T core.TimeMs, minZ int8) map[core.TileCoord]*core.TileImage {
	ctx := context.Background()
	out := map[core.TileCoord]*core.TileImage{}
	level := map[core.TileCoord]*core.TileImage{}
	for _, ti := range must[[]core.TileInfo](t)(ter.ListTiles(ctx, ds, 0)) {
		if ti.Version == 0 {
			continue
		}
		st := must[core.TileState](t)(ter.VersionAt(ctx, ds, ti.Tile, T))
		level[ti.Tile] = must[*core.TileImage](t)(ter.ReadVersion(ctx, ds, ti.Tile, st.Version))
	}
	for z := int8(-1); z >= minZ; z-- {
		parents := map[core.TileCoord]bool{}
		for c := range level {
			parents[c.Parent()] = true
		}
		next := map[core.TileCoord]*core.TileImage{}
		for pc := range parents {
			var kids [4]*core.TileImage
			for i, c := range pc.Children() {
				kids[i] = level[c]
			}
			next[pc] = core.Downsample(kids)
			out[pc] = next[pc]
		}
		level = next
	}
	return out
}

// checkVersions verifies every coarse version against the z0 content at its effective
// time, VersionAt around each version, and the per-level spacing.
func checkVersions(t *testing.T, ter *memstore.Terrain, p *Pyramid, minZ int8, intervals []core.TimeMs) int {
	t.Helper()
	ctx := context.Background()
	cache := map[core.TimeMs]map[core.TileCoord]*core.TileImage{}
	total := 0
	for z := int8(-1); z >= minZ; z-- {
		iv := intervals[min(int(-z), len(intervals))-1]
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
			hist := must[[]core.VersionPoint](t)(p.History(ctx, ds, ti.Tile, math.MinInt64, math.MaxInt64))
			if len(hist) != int(ti.Version) || hist[0].T != ti.FirstT || hist[len(hist)-1].T != ti.LastT {
				t.Fatalf("%v: history %d points, info %+v", ti.Tile, len(hist), ti)
			}
			for i, pt := range hist {
				total++
				if pt.Version != core.Version(i+1) {
					t.Fatalf("%v: point %d has version %d", ti.Tile, i, pt.Version)
				}
				// At most one version per aligned interval, except the final flush.
				if i > 0 && i < len(hist)-1 && core.FloorDiv(int64(pt.T), int64(iv)) <= core.FloorDiv(int64(hist[i-1].T), int64(iv)) {
					t.Fatalf("%v: versions %d and %d in the same %d ms interval", ti.Tile, i, i+1, iv)
				}
				if st := must[core.TileState](t)(p.VersionAt(ctx, ds, ti.Tile, pt.T)); st.Version != pt.Version || st.Live {
					t.Fatalf("%v: VersionAt(%d) = %+v, want %d", ti.Tile, pt.T, st, pt.Version)
				}
				if st := must[core.TileState](t)(p.VersionAt(ctx, ds, ti.Tile, pt.T-1)); st.Version != pt.Version-1 {
					t.Fatalf("%v: VersionAt(%d-1) = %+v, want %d", ti.Tile, pt.T, st, pt.Version-1)
				}
				exp := cache[pt.T]
				if exp == nil {
					exp = expectAt(t, ter, pt.T, minZ)
					cache[pt.T] = exp
				}
				want := exp[ti.Tile]
				if want == nil {
					want = core.NewTileImage()
				}
				got := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, pt.Version))
				if !got.Equal(want) {
					t.Fatalf("%v v%d (T=%d): content differs from the average of z0 at T", ti.Tile, pt.Version, pt.T)
				}
			}
		}
	}
	return total
}

func newPyramid(t testing.TB, ter core.TerrainStore, ss core.SeriesStore, opt Options) *Pyramid {
	p, err := New(context.Background(), ter, ss, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestLatestContract(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	h := genHistory(3, 200)
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{MinZ: -6})
	replayLive(t, ter, p, h, 0)
	if _, err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	coretest.PyramidLatest(t, ter, p, ds, -6)
}

// TestFullDepth checks every level down to core.MinZ against the reference average.
func TestFullDepth(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	h := genHistory(4, 60)
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
	replayLive(t, ter, p, h, 0)
	if _, err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	exp := expectAt(t, ter, math.MaxInt64, core.MinZ)
	n := 0
	for z := int8(-1); z >= core.MinZ; z-- {
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
			got := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, ti.Version))
			if want := exp[ti.Tile]; want == nil || !got.Equal(want) {
				t.Fatalf("%v latest differs", ti.Tile)
			}
			n++
		}
	}
	if n != len(exp) {
		t.Fatalf("pyramid has %d tiles, expected %d", n, len(exp))
	}
	if _, err := p.ListTiles(ctx, ds, core.MinZ-1); !errors.Is(err, core.ErrBadTile) {
		t.Fatalf("level below MinZ err = %v", err)
	}
}

func TestLiveMatchesZ0AtEveryVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		opt  Options
	}{
		{"default", Options{MinZ: -5}},
		{"every-pass-short-chain", Options{MinZ: -4, Intervals: []core.TimeMs{1}, MaxChain: 3}},
		{"custom", Options{MinZ: -4, Intervals: []core.TimeMs{2 * W, 6 * W}, SnapshotRatio: 0.5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ter := memstore.NewTerrain()
			p := newPyramid(t, ter, memstore.NewSeriesStore(), c.opt)
			replayLive(t, ter, p, genHistory(5, 600), 0)
			if _, err := p.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			iv := c.opt.Intervals
			if iv == nil {
				iv = DefaultIntervals
			}
			n := checkVersions(t, ter, p, c.opt.MinZ, iv)
			t.Logf("%d coarse versions checked", n)
			checkChains(t, p, c.opt.MinZ, max(c.opt.MaxChain, 0))
		})
	}
}

// checkChains verifies that no coarse series has more than maxChain deltas between
// snapshots and that each starts with a snapshot.
func checkChains(t *testing.T, p *Pyramid, minZ int8, maxChain int) {
	t.Helper()
	if maxChain == 0 {
		maxChain = 256
	}
	p.mu.RLock()
	d := p.ds[ds]
	p.mu.RUnlock()
	snaps, deltas := 0, 0
	for _, ct := range d.tiles {
		sr, n, _, _ := ct.state()
		run := 0
		for s := uint64(0); s < n; s++ {
			m := must[core.RecordMeta](t)(sr.Meta(s))
			if s == 0 && m.Kind != core.RecordSnapshot {
				t.Fatalf("%v: first record is kind %d", ct.coord, m.Kind)
			}
			if m.Kind == core.RecordSnapshot {
				run = 0
				snaps++
			} else {
				run++
				deltas++
				if m.Kind != core.RecordDelta || m.Flags != core.FlagWide || m.TLen != 0 {
					t.Fatalf("%v: bad delta meta %+v", ct.coord, m)
				}
			}
			if run > maxChain {
				t.Fatalf("%v: %d deltas after a snapshot", ct.coord, run)
			}
		}
	}
	t.Logf("%d snapshots, %d deltas", snaps, deltas)
	if deltas == 0 {
		t.Fatal("no deltas written")
	}
}

// TestImmutability: once the horizon passed t, VersionAt(t) and the content of every
// version never change, whatever is ingested later.
func TestImmutability(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{MinZ: -4})
	h := genHistory(6, 400)
	half := len(h.windows) / 2
	replayLive(t, ter, &(*p), history{windows: h.windows[:half]}, 0)
	type obs struct {
		tile core.TileCoord
		t    core.TimeMs
		v    core.Version
		im   *core.TileImage
	}
	horizon := t0 + core.TimeMs(half)*W
	r := rand.New(rand.NewSource(7))
	var seen []obs
	for z := int8(-1); z >= -4; z-- {
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
			for i := 0; i < 5; i++ {
				tt := t0 + core.TimeMs(r.Int63n(int64(horizon-t0)))
				st := must[core.TileState](t)(p.VersionAt(ctx, ds, ti.Tile, tt))
				seen = append(seen, obs{ti.Tile, tt, st.Version, must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, st.Version))})
			}
		}
	}
	if bt, ok := p.BuiltUpTo(ds); !ok || bt != horizon {
		t.Fatalf("BuiltUpTo = %v %v, want %v", bt, ok, horizon)
	}
	replayLive(t, ter, p, h, half)
	if _, err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	p.PurgeCache()
	for _, o := range seen {
		st := must[core.TileState](t)(p.VersionAt(ctx, ds, o.tile, o.t))
		im := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, o.tile, o.v))
		if st.Version != o.v || !im.Equal(o.im) {
			t.Fatalf("%v at %d changed: v%d -> v%d", o.tile, o.t, o.v, st.Version)
		}
	}
	t.Logf("%d observations unchanged", len(seen))
}

// TestRebuildEqualsLive: an offline Rebuild of a finished history gives the same
// versions (times and content) as Update after every commit plus a final Flush.
func TestRebuildEqualsLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ter := memstore.NewTerrain()
	opt := Options{MinZ: -5}
	live := newPyramid(t, ter, memstore.NewSeriesStore(), opt)
	replayLive(t, ter, live, genHistory(8, 900), 0)
	ls := must[BuildStats](t)(live.Flush(ctx))

	reb := newPyramid(t, ter, memstore.NewSeriesStore(), opt)
	rs := must[BuildStats](t)(reb.Rebuild(ctx, ds))
	t.Logf("rebuild: %+v (live flush %+v)", rs, ls)
	if _, err := reb.Rebuild(ctx, ds); !errors.Is(err, ErrAlreadyBuilt) {
		t.Fatalf("second rebuild err = %v", err)
	}
	compared := 0
	for z := int8(-1); z >= opt.MinZ; z-- {
		a := must[[]core.TileInfo](t)(live.ListTiles(ctx, ds, z))
		b := must[[]core.TileInfo](t)(reb.ListTiles(ctx, ds, z))
		if len(a) != len(b) {
			t.Fatalf("z%d: live %d tiles, rebuild %d", z, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("z%d: tile info live %+v, rebuild %+v", z, a[i], b[i])
			}
			ha := must[[]core.VersionPoint](t)(live.History(ctx, ds, a[i].Tile, math.MinInt64, math.MaxInt64))
			hb := must[[]core.VersionPoint](t)(reb.History(ctx, ds, b[i].Tile, math.MinInt64, math.MaxInt64))
			for j := range ha {
				if ha[j] != hb[j] {
					t.Fatalf("%v: version %d live %+v rebuild %+v", a[i].Tile, j+1, ha[j], hb[j])
				}
				ia := must[*core.TileImage](t)(live.ReadVersion(ctx, ds, a[i].Tile, ha[j].Version))
				ib := must[*core.TileImage](t)(reb.ReadVersion(ctx, ds, a[i].Tile, ha[j].Version))
				if !ia.Equal(ib) {
					t.Fatalf("%v v%d: content differs", a[i].Tile, ha[j].Version)
				}
				compared++
			}
		}
	}
	t.Logf("%d versions identical", compared)
	checkVersions(t, ter, reb, opt.MinZ, DefaultIntervals)
}

// TestReopen: a pyramid reopened over the same series resumes: same versions, same
// keyframe state, and further builds stay correct.
func TestReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ter := memstore.NewTerrain()
	ss := memstore.NewSeriesStore()
	opt := Options{MinZ: -4, Intervals: []core.TimeMs{1}, MaxChain: 5}
	h := genHistory(9, 500)
	p1 := must[*Pyramid](t)(New(ctx, ter, ss, opt))
	replayLive(t, ter, p1, history{windows: h.windows[:250]}, 0)
	before := must[[]core.TileInfo](t)(p1.ListTiles(ctx, ds, -2))
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.VersionAt(ctx, ds, before[0].Tile, 0); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("closed pyramid err = %v", err)
	}
	p2 := newPyramid(t, ter, ss, opt)
	after := must[[]core.TileInfo](t)(p2.ListTiles(ctx, ds, -2))
	if len(before) != len(after) || before[0] != after[0] {
		t.Fatalf("reopened tiles differ: %+v vs %+v", before, after)
	}
	replayLive(t, ter, p2, h, 250)
	if _, err := p2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	checkVersions(t, ter, p2, opt.MinZ, opt.Intervals)
	checkChains(t, p2, opt.MinZ, opt.MaxChain)
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{MinZ: -4})
	if _, err := p.VersionAt(ctx, ds, core.TileCoord{Z: -1}, 0); !errors.Is(err, core.ErrUnknownDatastore) {
		t.Fatalf("unknown datastore before data err = %v", err)
	}
	replayLive(t, ter, p, genHistory(10, 50), 0)
	must[BuildStats](t)(p.Flush(ctx))
	tiles := must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, -1))
	tile := tiles[0].Tile
	for _, c := range []struct {
		tile core.TileCoord
		key  core.DatastoreKey
		want error
	}{
		{core.TileCoord{Z: -5}, ds, core.ErrBadTile},                // below MinZ
		{core.TileCoord{Z: -1, X: 5000}, ds, core.ErrBadTile},       // outside the grid
		{core.TileCoord{Z: 1}, ds, core.ErrBadTile},                 // z > 0
		{core.TileCoord{Z: -1, X: 500, Y: 500}, ds, core.ErrNoTile}, // empty
		{tile, core.DatastoreKey{Map: "a", Surface: "b", Force: "c"}, core.ErrUnknownDatastore},
	} {
		if _, err := p.VersionAt(ctx, c.key, c.tile, 0); !errors.Is(err, c.want) {
			t.Errorf("VersionAt(%v %v) err = %v, want %v", c.key, c.tile, err, c.want)
		}
		if _, err := p.ReadVersion(ctx, c.key, c.tile, 1); !errors.Is(err, c.want) {
			t.Errorf("ReadVersion(%v %v) err = %v, want %v", c.key, c.tile, err, c.want)
		}
		if _, err := p.History(ctx, c.key, c.tile, 0, 1); !errors.Is(err, c.want) {
			t.Errorf("History(%v %v) err = %v, want %v", c.key, c.tile, err, c.want)
		}
	}
	if _, err := p.ReadVersion(ctx, ds, tile, tiles[0].Version+1); !errors.Is(err, core.ErrNoVersion) {
		t.Fatalf("version above latest err = %v", err)
	}
	if im := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, tile, 0)); !im.IsEmpty() {
		t.Fatal("version 0 not empty")
	}
	// z0 is delegated.
	z0 := must[[]core.TileInfo](t)(ter.ListTiles(ctx, ds, 0))
	if got := must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, 0)); len(got) != len(z0) {
		t.Fatal("z0 ListTiles not delegated")
	}
	// The returned image is the caller's: modifying it does not change later reads.
	a := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, tile, 1))
	a.Pix[0] ^= 0xFFFF
	if b := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, tile, 1)); b.Pix[0] == a.Pix[0] {
		t.Fatal("ReadVersion returned shared memory")
	}
}

// TestHostile: a million random coarse requests for invalid, empty or unknown tiles
// leave no lasting allocations.
func TestHostile(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
	replayLive(t, ter, p, genHistory(11, 30), 0)
	must[BuildStats](t)(p.Flush(ctx))
	bad := core.DatastoreKey{Map: "nope", Surface: "x", Force: "y"}
	r := rand.New(rand.NewSource(12))
	n := 1_000_000
	if testing.Short() {
		n = 100_000
	}
	p.mu.RLock()
	tilesBefore := len(p.ds[ds].tiles)
	p.mu.RUnlock()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for i := 0; i < n; i++ {
		tile := core.TileCoord{Z: int8(-1 - r.Intn(12)), X: int32(r.Intn(1<<16) - 1<<15), Y: int32(r.Intn(1<<16) - 1<<15)}
		key := ds
		if i%2 == 0 {
			key = bad
		}
		if tile.Valid() && tile.Z >= core.MinZ && key == ds {
			if _, err := p.VersionAt(ctx, key, tile, core.TimeMs(i)); err == nil && (tile.X > 4 || tile.X < -5) {
				t.Fatalf("%v served", tile)
			}
			continue
		}
		if _, err := p.ReadVersion(ctx, key, tile, 1); err == nil {
			t.Fatalf("%v %v served", key, tile)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	grow := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	p.mu.RLock()
	tilesAfter, dsCount := len(p.ds[ds].tiles), len(p.ds)
	p.mu.RUnlock()
	t.Logf("%d hostile requests: heap %+d bytes", n, grow)
	if grow > 2<<20 || tilesAfter != tilesBefore || dsCount != 1 {
		t.Fatalf("heap grew %d bytes, tiles %d -> %d, datastores %d", grow, tilesBefore, tilesAfter, dsCount)
	}
}

// TestConcurrentReads runs readers while the pyramid builds. Every successful read must
// match the reference average at the version's time (checked afterwards).
func TestConcurrentReads(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{MinZ: -3, Intervals: []core.TimeMs{1}, CacheBytes: 2 << 20})
	h := genHistory(13, 300)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	type read struct {
		tile core.TileCoord
		v    core.Version
		im   *core.TileImage
	}
	var reads []read
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				tile := core.TileCoord{Z: int8(-1 - r.Intn(3)), X: int32(r.Intn(4) - 2), Y: int32(r.Intn(4) - 2)}
				st, err := p.VersionAt(ctx, ds, tile, math.MaxInt64)
				if err != nil {
					if !errors.Is(err, core.ErrNoTile) && !errors.Is(err, core.ErrUnknownDatastore) {
						t.Error(err)
						return
					}
					continue
				}
				im, err := p.ReadVersion(ctx, ds, tile, st.Version)
				if err != nil {
					t.Error(err)
					return
				}
				if r.Intn(20) == 0 {
					mu.Lock()
					reads = append(reads, read{tile, st.Version, im})
					mu.Unlock()
				}
			}
		}(g)
	}
	replayLive(t, ter, p, h, 0)
	close(stop)
	wg.Wait()
	for _, rd := range reads {
		want := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, rd.tile, rd.v))
		if !want.Equal(rd.im) {
			t.Fatalf("%v v%d read during build differs", rd.tile, rd.v)
		}
	}
	t.Logf("%d concurrent reads verified", len(reads))
}
