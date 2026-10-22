// Integration tests against WP2's file-backed stores (internal/terrain and
// internal/series). TestWholeMapFileStores needs TSDB_TESTDATA.

package pyramid

import (
	"context"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
	"timeshadedb/internal/series"
	"timeshadedb/internal/terrain"
	"timeshadedb/internal/terrain/replay"
)

func openTerrain(t testing.TB, dir string) *terrain.Store {
	s, err := terrain.Open(dir, terrain.Options{NoSync: os.Getenv("TSDB_SYNC") != "1"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// inUse returns the number of series held by an open handle.
func inUse(s *series.Store) int {
	st := s.Stats()
	return st.Open - st.Idle
}

// TestFileStores runs a live history through the file-backed terrain store with the
// pyramid on the same series store, reopens both mid-way, and checks every coarse
// version, the latest state and that the pyramid holds no series handles.
func TestFileStores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	opt := Options{MinZ: -5, Intervals: []core.TimeMs{2 * W, 4 * W}}
	h := genHistory(31, 400)
	half := len(h.windows) / 2

	ter := openTerrain(t, dir)
	p := must[*Pyramid](t)(New(ctx, ter, ter.SeriesStore(), opt))
	replayLive(t, ter, p, history{windows: h.windows[:half]}, 0)
	if n := inUse(ter.SeriesStore()); n != 0 {
		t.Fatalf("%d series in use after live builds", n)
	}
	before := must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, -2))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ter.Close(); err != nil {
		t.Fatal(err)
	}

	ter = openTerrain(t, dir)
	defer ter.Close()
	p = must[*Pyramid](t)(New(ctx, ter, ter.SeriesStore(), opt))
	defer p.Close()
	after := must[[]core.TileInfo](t)(p.ListTiles(ctx, ds, -2))
	if len(before) != len(after) {
		t.Fatalf("reopened: %d tiles, before %d", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("reopened tile %+v, before %+v", after[i], before[i])
		}
	}
	replayLive(t, ter, p, h, half)
	must[BuildStats](t)(p.Flush(ctx))
	n := checkVersions(t, ter, p, opt.MinZ, opt.Intervals)
	coretest.PyramidLatest(t, ter, p, ds, opt.MinZ)
	if n := inUse(ter.SeriesStore()); n != 0 {
		t.Fatalf("%d series in use after reads", n)
	}
	t.Logf("%d coarse versions checked on file stores; series stats %+v", n, ter.SeriesStore().Stats())
}

func dirSize(dir string, match func(string) bool) int64 {
	var n int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && match(filepath.ToSlash(p)) {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// TestWholeMapFileStores replays the illisis test data (TSDB_TESTDATA) through the
// file-backed terrain store the way WP2's replay benchmark does (a Commit before every
// batch of 1024 updates), with Pyramid.Update after every Commit (live mode) and a
// Flush at the end. It reports z0 and pyramid disk use per chunk-hour over the whole
// map (PLAN.md §5: <= 5 B), Update timings and series memory, and checks the latest
// coarse state against the z0 average. TSDB_SYNC=1 enables fsync.
func TestWholeMapFileStores(t *testing.T) {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set")
	}
	ctx := context.Background()
	dir := os.Getenv("TSDB_INTEGRATION_DB")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	areas, err := replay.Scan(root, func(core.DatastoreKey) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := replay.NewStream(areas)
	if err != nil {
		t.Fatal(err)
	}
	ter := openTerrain(t, dir)
	p := must[*Pyramid](t)(New(ctx, ter, ter.SeriesStore(), Options{}))
	type span struct{ first, last core.TimeMs }
	spans := map[core.DatastoreKey]*span{}
	const batch = 1024
	ups := make([]core.ChunkUpdate, batch)
	bufs := make([]core.ChunkPixels, batch)
	var lastT core.TimeMs
	var updates []time.Duration
	var total BuildStats
	maxInUse := 0
	for {
		n := 0
		for n < batch {
			ok, err := stream.Next(&ups[n], &bufs[n])
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			ups[n].Pixels = &bufs[n]
			sp := spans[ups[n].DS]
			if sp == nil {
				sp = &span{ups[n].T, ups[n].T}
				spans[ups[n].DS] = sp
			}
			sp.last = max(sp.last, ups[n].T)
			lastT = max(lastT, ups[n].T)
			n++
		}
		if n == 0 {
			break
		}
		if err := ter.Commit(ctx, ups[0].T); err != nil {
			t.Fatal(err)
		}
		start := hrNow()
		st, err := p.Update(ctx)
		updates = append(updates, hrSince(start))
		if err != nil {
			t.Fatal(err)
		}
		total.add(st)
		maxInUse = max(maxInUse, inUse(ter.SeriesStore()))
		if _, err := ter.Apply(ctx, ups[:n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := ter.Commit(ctx, lastT+W); err != nil {
		t.Fatal(err)
	}
	must[BuildStats](t)(p.Update(ctx))
	fl := must[BuildStats](t)(p.Flush(ctx))
	total.add(fl)
	t.Logf("live: %d Updates p50 %v p99 %v max %v; built %+v; series in use after an Update: max %d",
		len(updates), pct(updates, 0.5), pct(updates, 0.99), slicesMax(updates), total, maxInUse)
	if maxInUse != 0 {
		t.Errorf("the pyramid left %d series in use", maxInUse)
	}

	// The latest coarse state equals the average of the latest z0 content.
	checked := 0
	for _, info := range must[[]core.DatastoreInfo](t)(ter.Datastores(ctx)) {
		exp := expectAtKey(t, ter, info.Key, math.MaxInt64, core.MinZ)
		for z := int8(-1); z >= core.MinZ; z-- {
			for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, info.Key, z)) {
				got := must[*core.TileImage](t)(p.ReadVersion(ctx, info.Key, ti.Tile, ti.Version))
				if want := exp[ti.Tile]; want == nil || !got.Equal(want) {
					t.Fatalf("%v %v: latest differs from the z0 average", info.Key, ti.Tile)
				}
				checked++
			}
		}
	}
	t.Logf("%d latest coarse tiles equal the z0 average", checked)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ter.Close(); err != nil {
		t.Fatal(err)
	}

	// Disk use per datastore: z0 (tiles/ and palette.bin) and pyramid/.
	final := stream.FinalState()
	var z0All, pyrAll int64
	var chAll float64
	dss, _ := os.ReadDir(filepath.Join(dir, "datastores"))
	for _, e := range dss {
		key, err := core.ParseDatastoreID(e.Name())
		if err != nil || spans[key] == nil {
			continue
		}
		dp := filepath.Join(dir, "datastores", e.Name())
		z0 := dirSize(dp, func(f string) bool {
			return strings.Contains(f, "/tiles/") && (strings.HasSuffix(f, ".blk") || strings.HasSuffix(f, ".idx")) || strings.HasSuffix(f, "palette.bin")
		})
		pyr := dirSize(filepath.Join(dp, "pyramid"), func(f string) bool { return strings.HasSuffix(f, ".blk") || strings.HasSuffix(f, ".idx") })
		ch := float64(len(final[key])) * float64(spans[key].last-spans[key].first) / float64(core.Hour)
		if ch > 0 {
			t.Logf("%-44s z0 %9d B %7.3f B/ch  pyramid %8d B %7.3f B/ch", e.Name(), z0, float64(z0)/ch, pyr, float64(pyr)/ch)
		}
		z0All += z0
		pyrAll += pyr
		chAll += ch
	}
	t.Logf("ALL: z0 %d B = %.3f B/chunk-hour, pyramid %d B = %.3f, total %.3f B/chunk-hour (PLAN.md §5 target <= 5)",
		z0All, float64(z0All)/chAll, pyrAll, float64(pyrAll)/chAll, float64(z0All+pyrAll)/chAll)

	// Reopen: series memory with the pyramid loaded (its tiles hold no handles).
	ter = openTerrain(t, dir)
	defer ter.Close()
	p = must[*Pyramid](t)(New(ctx, ter, ter.SeriesStore(), Options{}))
	defer p.Close()
	t.Logf("after reopen: series stats %+v", ter.SeriesStore().Stats())
	if n := inUse(ter.SeriesStore()); n != 0 {
		t.Errorf("%d series in use after opening the pyramid", n)
	}
}

func slicesMax(d []time.Duration) time.Duration {
	var m time.Duration
	for _, x := range d {
		m = max(m, x)
	}
	return m
}

// expectAtKey is expectAt for any datastore.
func expectAtKey(t testing.TB, ter core.TerrainStore, key core.DatastoreKey, T core.TimeMs, minZ int8) map[core.TileCoord]*core.TileImage {
	ctx := context.Background()
	out := map[core.TileCoord]*core.TileImage{}
	level := map[core.TileCoord]*core.TileImage{}
	for _, ti := range must[[]core.TileInfo](t)(ter.ListTiles(ctx, key, 0)) {
		if ti.Version == 0 {
			continue
		}
		st := must[core.TileState](t)(ter.VersionAt(ctx, key, ti.Tile, T))
		level[ti.Tile] = must[*core.TileImage](t)(ter.ReadVersion(ctx, key, ti.Tile, st.Version))
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
