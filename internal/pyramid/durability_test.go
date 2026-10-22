package pyramid

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/memstore"
	"timeshadedb/internal/core/wire"
	"timeshadedb/internal/tileenc"
)

// trackStore wraps a core.SeriesStore to check how the pyramid uses series: open
// handles (the file store keeps the index of a series in memory while a handle is
// open), the synced length of every series (records past it are lost in a crash), and
// injected Sync and Append failures.
type trackStore struct {
	core.SeriesStore
	open       atomic.Int64
	opens      atomic.Int64
	mu         sync.Mutex
	synced     map[string]uint64
	failSync   atomic.Int64 // fail the next n Sync calls
	failAppend atomic.Int64 // fail the next n Append calls
}

type trackSeries struct {
	core.Series
	st     *trackStore
	name   string
	closed bool
}

var errInjected = errors.New("injected failure")

func newTrackStore() *trackStore {
	return &trackStore{SeriesStore: memstore.NewSeriesStore(), synced: map[string]uint64{}}
}

func (s *trackStore) Open(ctx context.Context, name string, create bool) (core.Series, error) {
	sr, err := s.SeriesStore.Open(ctx, name, create)
	if err != nil {
		return nil, err
	}
	s.open.Add(1)
	s.opens.Add(1)
	return &trackSeries{Series: sr, st: s, name: name}, nil
}

func (s *trackStore) syncedLen(name string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[name]
}

func (h *trackSeries) Append(ctx context.Context, rec core.Record) (uint64, error) {
	if h.st.failAppend.Add(-1) >= 0 {
		return 0, errInjected
	}
	return h.Series.Append(ctx, rec)
}

func (h *trackSeries) Sync() error {
	if h.st.failSync.Add(-1) >= 0 {
		return errInjected
	}
	h.st.mu.Lock()
	h.st.synced[h.name] = h.Series.Len()
	h.st.mu.Unlock()
	return h.Series.Sync()
}

func (h *trackSeries) Close() error {
	if !h.closed {
		h.closed = true
		h.st.open.Add(-1)
	}
	return h.Series.Close()
}

// checkPublishedSynced fails if any version a reader can see is not synced.
func checkPublishedSynced(t *testing.T, p *Pyramid, ss *trackStore, minZ int8) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	for z := int8(-1); z >= minZ; z-- {
		tiles, err := p.ListTiles(ctx, ds, z)
		if errors.Is(err, core.ErrUnknownDatastore) {
			return 0
		}
		for _, ti := range must[[]core.TileInfo](t)(tiles, err) {
			if s := ss.syncedLen(seriesName(ds.ID(), ti.Tile)); uint64(ti.Version) > s {
				t.Fatalf("%v: version %d visible, only %d synced", ti.Tile, ti.Version, s)
			}
			n++
		}
	}
	return n
}

// TestDurableAndHandleFree: every version a reader can see is synced, and no series
// handle stays open after builds or reads.
func TestDurableAndHandleFree(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	ss := newTrackStore()
	opt := Options{MinZ: -4, Intervals: []core.TimeMs{2 * W, 4 * W}}
	p := newPyramid(t, ter, ss, opt)
	h := genHistory(21, 300)
	for w := 0; w < len(h.windows); w++ {
		must[core.ApplyResult](t)(ter.Apply(ctx, h.windows[w]))
		if err := ter.Commit(ctx, t0+core.TimeMs(w+1)*W); err != nil {
			t.Fatal(err)
		}
		must[BuildStats](t)(p.Update(ctx))
		checkPublishedSynced(t, p, ss, opt.MinZ)
		if w%25 == 0 {
			for z := int8(-1); z >= opt.MinZ; z-- {
				for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
					must[core.TileState](t)(p.VersionAt(ctx, ds, ti.Tile, ti.FirstT))
					must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, 1))
					must[[]core.VersionPoint](t)(p.History(ctx, ds, ti.Tile, 0, math.MaxInt64))
					if _, _, err := p.ReadTile(ctx, ds, ti.Tile, math.MaxInt64, true); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if n := ss.open.Load(); n != 0 {
			t.Fatalf("window %d: %d series handles left open", w, n)
		}
	}
	must[BuildStats](t)(p.Flush(ctx))
	n := checkPublishedSynced(t, p, ss, opt.MinZ)
	if o := ss.open.Load(); o != 0 {
		t.Fatalf("%d series handles left open after flush", o)
	}
	checkVersions(t, ter, p, opt.MinZ, opt.Intervals)
	t.Logf("%d tiles, %d series opens, none left open", n, ss.opens.Load())
}

// TestSyncFailure: a failed Sync publishes nothing and does not advance BuiltUpTo; the
// next Update syncs and publishes the staged versions, and the result is the same as
// without failures.
func TestSyncFailure(t *testing.T) {
	ctx := context.Background()
	opt := Options{MinZ: -4, Intervals: []core.TimeMs{1}}
	h := genHistory(22, 200)

	refTer := memstore.NewTerrain()
	ref := newPyramid(t, refTer, memstore.NewSeriesStore(), opt)
	replayLive(t, refTer, ref, h, 0)

	ter := memstore.NewTerrain()
	ss := newTrackStore()
	p := newPyramid(t, ter, ss, opt)
	failures := 0
	for w := 0; w < len(h.windows); w++ {
		must[core.ApplyResult](t)(ter.Apply(ctx, h.windows[w]))
		if err := ter.Commit(ctx, t0+core.TimeMs(w+1)*W); err != nil {
			t.Fatal(err)
		}
		before, hadBefore := p.BuiltUpTo(ds)
		inject := w%7 == 3
		if inject {
			ss.failSync.Store(1)
		}
		if w%11 == 5 {
			ss.failAppend.Store(1)
		}
		_, err := p.Update(ctx)
		failed := inject && ss.failSync.Load() <= 0
		ss.failSync.Store(0)
		appendFailed := w%11 == 5 && ss.failAppend.Load() <= 0
		ss.failAppend.Store(0)
		if failed || appendFailed {
			if !errors.Is(err, errInjected) {
				t.Fatalf("window %d: injected failure, Update err = %v", w, err)
			}
			failures++
			after, _ := p.BuiltUpTo(ds)
			if hadBefore && after != before {
				t.Fatalf("window %d: BuiltUpTo moved from %v to %v on a failed pass", w, before, after)
			}
		} else if err != nil {
			t.Fatalf("window %d: %v", w, err)
		}
		checkPublishedSynced(t, p, ss, opt.MinZ)
	}
	must[BuildStats](t)(p.Flush(ctx))
	must[BuildStats](t)(ref.Flush(ctx))
	checkPublishedSynced(t, p, ss, opt.MinZ)
	if failures == 0 {
		t.Fatal("no failure was injected")
	}
	// Same latest content as the reference without failures, and every version equals
	// the average of z0 at its time (failed passes only delay versions).
	exp := expectAt(t, ter, math.MaxInt64, opt.MinZ)
	for z := int8(-1); z >= opt.MinZ; z-- {
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
			got := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, ti.Version))
			if !got.Equal(exp[ti.Tile]) {
				t.Fatalf("%v: latest differs after failures", ti.Tile)
			}
		}
	}
	checkVersionsLoose(t, ter, p, opt.MinZ)
	t.Logf("%d injected failures recovered", failures)
}

// checkVersionsLoose checks every coarse version's content against the average of z0
// at its effective time (no spacing rule).
func checkVersionsLoose(t *testing.T, ter core.TerrainStore, p *Pyramid, minZ int8) {
	t.Helper()
	ctx := context.Background()
	for z := int8(-1); z >= minZ; z-- {
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
			for _, pt := range must[[]core.VersionPoint](t)(p.History(ctx, ds, ti.Tile, math.MinInt64, math.MaxInt64)) {
				want := expectAtDS(t, ter, ds, ti.Tile, pt.T)
				if got := must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, pt.Version)); !got.Equal(want) {
					t.Fatalf("%v v%d at %v differs from the z0 average", ti.Tile, pt.Version, pt.T)
				}
			}
		}
	}
}

// TestLatestEveryLevel: without any Flush, after every Update each coarse tile exists
// at every level as soon as a z0 descendant has data, and its live latest (ReadTile
// with live) equals the average of the committed z0 content at the horizon; Live is
// false only when the latest version already holds that content.
func TestLatestEveryLevel(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	minZ := int8(-5)
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{MinZ: minZ})
	r, err := tileenc.NewRenderer(p, ter, tileenc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h := genHistory(23, 400)
	checked, live, bodies := 0, 0, 0
	for w := 0; w < len(h.windows); w++ {
		must[core.ApplyResult](t)(ter.Apply(ctx, h.windows[w]))
		horizon := t0 + core.TimeMs(w+1)*W
		if err := ter.Commit(ctx, horizon); err != nil {
			t.Fatal(err)
		}
		must[BuildStats](t)(p.Update(ctx))
		if w%13 != 0 && w != len(h.windows)-1 || len(must[[]core.DatastoreInfo](t)(ter.Datastores(ctx))) == 0 {
			continue
		}
		exp := expectAt(t, ter, horizon, minZ)
		n := 0
		for z := int8(-1); z >= minZ; z-- {
			for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, z)) {
				n++
				want := exp[ti.Tile]
				st := must[core.TileState](t)(p.VersionAt(ctx, ds, ti.Tile, horizon))
				im, st2, err := p.ReadTile(ctx, ds, ti.Tile, horizon, true)
				if err != nil {
					t.Fatal(err)
				}
				if st != st2 || st.Live != ti.Live {
					t.Fatalf("%v: VersionAt %+v, ReadTile %+v, ListTiles live %v", ti.Tile, st, st2, ti.Live)
				}
				if !im.Equal(want) {
					t.Fatalf("window %d %v: latest (live %v) differs from the z0 average at the horizon", w, ti.Tile, st.Live)
				}
				if st.Live && bodies < 50 {
					// The renderer serves the same live content (identity encoding).
					b, bst, err := r.LiveTile(ctx, ds, ti.Tile, horizon, tileenc.Identity)
					if err != nil {
						t.Fatal(err)
					}
					hd, bim, err := wire.DecodeTile(b.Data, nil)
					if err != nil || !hd.Live || hd.Z != ti.Tile.Z || hd.Version != st.Version || bst != st || !bim.Equal(want) {
						t.Fatalf("%v: live body %+v %+v err %v", ti.Tile, hd, bst, err)
					}
					bodies++
				}
				if st.Live {
					live++
					if st.LiveSeq == 0 {
						t.Fatalf("%v: live without LiveSeq", ti.Tile)
					}
				} else if st.Version != ti.Version {
					t.Fatalf("%v: not live at the horizon but version %d of %d", ti.Tile, st.Version, ti.Version)
				}
				// Without live, ReadTile is the committed version.
				com, _, err := p.ReadTile(ctx, ds, ti.Tile, horizon, false)
				if err != nil {
					t.Fatal(err)
				}
				if !com.Equal(must[*core.TileImage](t)(p.ReadVersion(ctx, ds, ti.Tile, st.Version))) {
					t.Fatalf("%v: ReadTile without live is not the committed version", ti.Tile)
				}
				checked++
			}
		}
		if n != len(exp) {
			t.Fatalf("window %d: pyramid lists %d coarse tiles, z0 has data under %d", w, n, len(exp))
		}
	}
	if live == 0 {
		t.Fatal("no live latest seen")
	}
	t.Logf("%d latest checks, %d live, %d live bodies", checked, live, bodies)
}
