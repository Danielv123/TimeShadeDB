package overlays

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

func open(t testing.TB, dir string, opts Options) *Store {
	t.Helper()
	o, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOverlayContract(t *testing.T) {
	coretest.OverlayStore(t, func(t *testing.T) core.OverlayStore { return open(t, t.TempDir(), Options{}) })
}

func TestOverlayContractTinyCache(t *testing.T) {
	coretest.OverlayStore(t, func(t *testing.T) core.OverlayStore {
		return open(t, t.TempDir(), Options{NoSync: true, CacheBytes: 1})
	})
}

var dss = []core.DatastoreKey{coretest.DS, {Map: "gw-a.1", Surface: "Gleba", Force: "player"}}

// randomEvents makes tags, recipes and rails in a few tiles that open, change, move
// between tiles and close, with late and invalid events mixed in.
func randomEvents(rng *rand.Rand, t0 core.TimeMs, n int) []core.OverlayEvent {
	kinds := []core.IntervalKind{core.OverlayTag, core.OverlayRecipe, core.OverlayRail}
	var out []core.OverlayEvent
	t := t0
	for i := 0; i < n; i++ {
		t += core.TimeMs(rng.Intn(5000))
		k := kinds[rng.Intn(3)]
		key := fmt.Sprintf("%d/%d", 1+rng.Intn(2), rng.Intn(25))
		iv := core.Interval{Kind: k, Key: key, Instance: 1, X: float64(rng.Intn(1500) - 750), Y: float64(rng.Intn(1500)-750) + 0.5}
		switch k {
		case core.OverlayTag:
			iv.Icon, iv.Text = "item/iron-ore", fmt.Sprint(rng.Intn(3))
		case core.OverlayRecipe:
			iv.Recipe, iv.Quality = []string{"iron-plate", "steel-plate"}[rng.Intn(2)], "normal"
		case core.OverlayRail:
			iv.Name, iv.Direction = "straight-rail", uint8(rng.Intn(16))
		}
		if rng.Intn(3) > 0 { // usually the object stays where it is
			iv.X, iv.Y = float64(len(key)*37), float64(len(key)*11)+0.5
		}
		ev := core.OverlayEvent{DS: dss[rng.Intn(2)], T: t, Interval: iv}
		switch rng.Intn(12) {
		case 0, 1:
			ev.Close = true
		case 2:
			ev.T -= core.TimeMs(rng.Intn(20000)) // possibly late
		case 3:
			ev.Interval.Kind = "bogus"
		case 4:
			ev.Interval.X = 1e12 // off the grid
		}
		out = append(out, ev)
		if rng.Intn(8) == 0 {
			out = append(out, ev) // same content again: a no-op
		}
	}
	return out
}

func compare(t *testing.T, ctx context.Context, a, b core.OverlayStore, t1 core.TimeMs) {
	t.Helper()
	for _, ds := range dss {
		for x := int32(-3); x <= 3; x++ {
			for y := int32(-3); y <= 3; y++ {
				tile := core.TileCoord{X: x, Y: y}
				va, err := a.Version(ctx, ds, tile)
				if err != nil {
					t.Fatal(err)
				}
				vb, _ := b.Version(ctx, ds, tile)
				if va != vb {
					t.Fatalf("version %v %v = %d, want %d", ds, tile, va, vb)
				}
				for _, q := range []core.OverlayQuery{
					{Tile: tile, T0: 0, T1: t1 + 1},
					{Tile: tile, T0: t1 / 2},
					{Tile: tile, T0: t1 - 20000, T1: t1, Kinds: []core.IntervalKind{core.OverlayRail}},
					{Tile: tile, T0: t1 / 3, T1: t1 / 2, Kinds: []core.IntervalKind{core.OverlayTag, core.OverlayRecipe}},
				} {
					ga, err := a.Query(ctx, ds, q)
					if err != nil {
						t.Fatal(err)
					}
					gb, _ := b.Query(ctx, ds, q)
					if !reflect.DeepEqual(ga, gb) {
						t.Fatalf("query %+v on %v:\n got  %+v\n want %+v", q, ds, ga, gb)
					}
				}
			}
		}
	}
}

// TestDifferential compares every query and version with memstore over random batches,
// reopening the store at random points.
func TestDifferential(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			opts := Options{NoSync: true, CacheBytes: int64(rng.Intn(20000) + 1)}
			a := open(t, dir, opts)
			defer func() { a.Close() }()
			b := memstore.NewOverlays()
			t0 := core.TimeMs(1_000_000)
			for step := 0; step < 25; step++ {
				batch := randomEvents(rng, t0, 60)
				t0 = batch[len(batch)-1].T
				ra, err := a.Apply(ctx, batch)
				if err != nil {
					t.Fatal(err)
				}
				rb, _ := b.Apply(ctx, batch)
				if ra != rb {
					t.Fatalf("step %d apply = %+v, want %+v", step, ra, rb)
				}
				if rng.Intn(4) == 0 {
					a.Close()
					a = open(t, dir, opts)
				}
				compare(t, ctx, a, b, t0)
			}
		})
	}
}

// TestTornTail cuts the log in the middle of the last record of a move (end of the
// old interval written, open of the new one torn): reopening keeps the end, drops the
// torn open, and appending afterwards works.
func TestTornTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	o := open(t, dir, Options{})
	tag := core.Interval{Kind: core.OverlayTag, Key: "1/1", X: 10, Y: 10, Text: "a"}
	o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: 100, Interval: tag}})
	path := filepath.Join(dir, "overlays", coretest.DS.ID(), "log.ivl")
	st, _ := os.Stat(path)
	moved := tag
	moved.X, moved.Text = 2000, "b"
	o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: 200, Interval: moved}})
	o.Close()
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, raw[:len(raw)-3], 0o644)
	o = open(t, dir, Options{})
	defer o.Close()
	old := core.TileAt(10, 10, 0)
	got, err := o.Query(ctx, coretest.DS, core.OverlayQuery{Tile: old, T0: 0, T1: 1000})
	if err != nil || len(got) != 1 || got[0].Text != "a" || got[0].End != 200 {
		t.Fatalf("after recovery: %+v, %v", got, err)
	}
	if v, _ := o.Version(ctx, coretest.DS, core.TileAt(2000, 10, 0)); v != 0 {
		t.Fatalf("torn open still indexed: version %d", v)
	}
	if fi, _ := os.Stat(path); fi.Size() <= st.Size() || fi.Size() >= int64(len(raw)) {
		t.Fatalf("log size %d, want between %d and %d", fi.Size(), st.Size(), len(raw))
	}
	res, err := o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: 300, Interval: moved}})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("apply after recovery = %+v, %v", res, err)
	}
	o.Close()
	o = open(t, dir, Options{})
	defer o.Close()
	got, _ = o.Query(ctx, coretest.DS, core.OverlayQuery{Tile: core.TileAt(2000, 10, 0), T0: 300})
	if len(got) != 1 || got[0].Text != "b" || got[0].Start != 300 {
		t.Fatalf("after append and reopen: %+v", got)
	}
}

// TestCorruptRecord flips a byte inside an early record. Valid records follow, so this
// is not a torn write: Open refuses the log instead of deleting them; with Salvage it
// keeps the damaged file aside and continues from the records before the damage.
func TestCorruptRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	o := open(t, dir, Options{NoSync: true})
	for i := 0; i < 10; i++ {
		o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: core.TimeMs(100 * (i + 1)), Interval: core.Interval{Kind: core.OverlayRecipe, Key: fmt.Sprint("1/", i), X: 5, Y: 5, Recipe: "iron-plate"}}})
	}
	o.Close()
	path := filepath.Join(dir, "overlays", coretest.DS.ID(), "log.ivl")
	raw, _ := os.ReadFile(path)
	raw[len(raw)/2] ^= 0x55
	os.WriteFile(path, raw, 0o644)
	if o, err := Open(dir, Options{NoSync: true}); !errors.Is(err, ErrCorrupt) {
		if o != nil {
			o.Close()
		}
		t.Fatalf("open of a log damaged in the middle = %v, want ErrCorrupt", err)
	}
	if after, _ := os.ReadFile(path); !reflect.DeepEqual(after, raw) {
		t.Fatal("a refused open changed the log")
	}
	o = open(t, dir, Options{NoSync: true, Salvage: true})
	defer o.Close()
	v, _ := o.Version(ctx, coretest.DS, core.TileAt(5, 5, 0))
	if v == 0 || v >= 10 || o.Stats().Salvaged != 1 {
		t.Fatalf("version after salvage = %d, stats %+v", v, o.Stats())
	}
	got, _ := o.Query(ctx, coretest.DS, core.OverlayQuery{Tile: core.TileAt(5, 5, 0), T0: 0, T1: 10000})
	if uint64(len(got)) != v {
		t.Fatalf("%d intervals for version %d", len(got), v)
	}
	aside, _ := filepath.Glob(path + ".damaged-*")
	if len(aside) != 1 {
		t.Fatalf("damaged copies: %v", aside)
	}
	if kept, _ := os.ReadFile(aside[0]); !reflect.DeepEqual(kept, raw) {
		t.Fatal("the damaged copy differs from the damaged log")
	}
}

// TestZeroTail: a crash can leave the end of a log zero-filled; that is a torn tail.
func TestZeroTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	o := open(t, dir, Options{NoSync: true})
	o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: 100, Interval: core.Interval{Kind: core.OverlayTag, Key: "1/1", X: 1, Y: 1, Text: "a"}}})
	o.Close()
	path := filepath.Join(dir, "overlays", coretest.DS.ID(), "log.ivl")
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, append(raw, make([]byte, 4096)...), 0o644)
	o = open(t, dir, Options{NoSync: true})
	defer o.Close()
	if fi, _ := os.Stat(path); fi.Size() != int64(len(raw)) || o.Stats().TornBytes != 4096 {
		t.Fatalf("size %d, want %d; stats %+v", fi.Size(), len(raw), o.Stats())
	}
	if v, _ := o.Version(ctx, coretest.DS, core.TileAt(1, 1, 0)); v != 1 {
		t.Fatalf("version = %d", v)
	}
}

// TestHostileApply: events that store nothing (closes of unknown keys, invalid events)
// create no datastore, directory or file handle.
func TestHostileApply(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	o := open(t, dir, Options{NoSync: true})
	defer o.Close()
	var evs []core.OverlayEvent
	for i := 0; i < 1000; i++ {
		ds := core.DatastoreKey{Map: fmt.Sprint("m", i), Surface: "s", Force: "f"}
		evs = append(evs, core.OverlayEvent{DS: ds, T: 1, Close: true, Interval: core.Interval{Kind: core.OverlayRail, Key: fmt.Sprint(i)}})
	}
	res, err := o.Apply(ctx, evs)
	if err != nil || res.Accepted != 1000 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if dss, _ := o.Datastores(ctx); len(dss) != 0 {
		t.Fatalf("datastores = %v", dss)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "overlays")); len(ents) != 0 {
		t.Fatalf("%d entries created on disk", len(ents))
	}
}

// TestHostileQueries: unknown datastores and tiles answer without allocating state.
func TestHostileQueries(t *testing.T) {
	ctx := context.Background()
	o := open(t, t.TempDir(), Options{NoSync: true})
	defer o.Close()
	o.Apply(ctx, []core.OverlayEvent{{DS: coretest.DS, T: 1, Interval: core.Interval{Kind: core.OverlayTag, Key: "1/1", X: 1, Y: 1}}})
	rng := rand.New(rand.NewSource(1))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 200_000; i++ {
		tile := core.TileCoord{X: int32(rng.Intn(4000) - 2000), Y: int32(rng.Intn(4000) - 2000)}
		ds := coretest.DS
		if i%2 == 0 {
			ds = core.DatastoreKey{Map: fmt.Sprint(i), Surface: "s", Force: "f"}
		}
		o.Query(ctx, ds, core.OverlayQuery{Tile: tile, T0: 0, T1: 10})
		o.Version(ctx, ds, tile)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grow := int64(after.HeapAlloc) - int64(before.HeapAlloc); grow > 1<<20 {
		t.Fatalf("heap grew by %d bytes", grow)
	}
	if st := o.Stats(); st.Tiles != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestRailsScale stores 200,000 rails over a 40 x 40 tile area, then queries tiles at
// a point in time and checks the cache bound, reopening time and memory per interval.
func TestRailsScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	dir := t.TempDir()
	o := open(t, dir, Options{NoSync: true, CacheBytes: 4 << 20})
	rng := rand.New(rand.NewSource(2))
	const n = 200_000
	var evs []core.OverlayEvent
	for i := 0; i < n; i++ {
		evs = append(evs, core.OverlayEvent{DS: coretest.DS, T: core.TimeMs(1000 + i), Interval: core.Interval{
			Kind: core.OverlayRail, Key: fmt.Sprintf("248495586/%d", 100000+i), Instance: 248495586,
			X: float64(rng.Intn(20480)-10240) + 1, Y: float64(rng.Intn(20480)-10240) + 1, Name: "straight-rail", Direction: uint8(rng.Intn(8) * 2)}})
	}
	t0 := time.Now()
	for i := 0; i < n; i += 5000 {
		if _, err := o.Apply(ctx, evs[i:i+5000]); err != nil {
			t.Fatal(err)
		}
	}
	apply := time.Since(t0)
	// Remove a tenth.
	var rm []core.OverlayEvent
	for i := 0; i < n; i += 10 {
		e := evs[i]
		e.Close, e.T = true, core.TimeMs(5_000_000+i)
		rm = append(rm, e)
	}
	o.Apply(ctx, rm)
	t1 := time.Now()
	total := 0
	for x := int32(-20); x <= 20; x++ {
		for y := int32(-20); y <= 20; y++ {
			got, err := o.Query(ctx, coretest.DS, core.OverlayQuery{Tile: core.TileCoord{X: x, Y: y}, T0: 6_000_000, Kinds: []core.IntervalKind{core.OverlayRail}})
			if err != nil {
				t.Fatal(err)
			}
			total += len(got)
		}
	}
	query := time.Since(t1)
	st := o.Stats()
	o.Close()
	evs, rm, o = nil, nil, nil
	// Heap of a freshly opened store: the index and the open map, no cache.
	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	t2 := time.Now()
	o = open(t, dir, Options{NoSync: true})
	reopen := time.Since(t2)
	runtime.GC()
	runtime.ReadMemStats(&ms1)
	defer o.Close()
	if total != n-n/10 || st.CacheBytes > 4<<20 {
		t.Fatalf("total %d, stats %+v", total, st)
	}
	t.Logf("NoSync (no fsync per Apply); TestRailsApplyFsync measures the fsync cost")
	t.Logf("%d rails: apply %v (%.0f events/s), heap after reopen %.1f MB (%.0f B per open interval), 1681 tile queries %v (%.0f µs each, cache %d KB), reopen %v",
		n, apply.Round(time.Millisecond), float64(n)/apply.Seconds(), float64(int64(ms1.HeapAlloc)-int64(ms0.HeapAlloc))/1e6,
		float64(int64(ms1.HeapAlloc)-int64(ms0.HeapAlloc))/float64(st.OpenIntervals), query.Round(time.Millisecond),
		float64(query.Microseconds())/1681, st.CacheBytes>>10, reopen.Round(time.Millisecond))
}

// TestRailsApplyFsync measures applying rails with an fsync per Apply (the default), in
// batches of 5,000 and of 100 events.
func TestRailsApplyFsync(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	for _, batch := range []int{5000, 100} {
		o := open(t, t.TempDir(), Options{})
		const n = 50_000
		var evs []core.OverlayEvent
		for i := 0; i < n; i++ {
			evs = append(evs, core.OverlayEvent{DS: coretest.DS, T: core.TimeMs(1000 + i), Interval: core.Interval{
				Kind: core.OverlayRail, Key: fmt.Sprintf("248495586/%d", 100000+i), Instance: 248495586,
				X: float64(i%2000) + 1, Y: float64(i/2000) + 1, Name: "straight-rail"}})
		}
		t0 := time.Now()
		for i := 0; i < n; i += batch {
			if _, err := o.Apply(ctx, evs[i:i+batch]); err != nil {
				t.Fatal(err)
			}
		}
		d := time.Since(t0)
		o.Close()
		t.Logf("fsync per Apply, batches of %d: %d rails in %v (%.0f events/s, %.2f ms per Apply)",
			batch, n, d.Round(time.Millisecond), float64(n)/d.Seconds(), float64(d.Microseconds())/1000/float64(n/batch))
	}
}
