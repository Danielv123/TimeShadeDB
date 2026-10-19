package pyramid

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
	"timeshadedb/internal/v3/core/wire"
	"timeshadedb/internal/v3/tileenc"
)

// loadHistory replays the full z0 history of a surface into a reference terrain in
// 60 s windows (the newest content of each touched chunk per window) and commits it.
func loadHistory(t testing.TB, s mmSurface) (ter *memstore.Terrain, records, chunks int, hours float64) {
	ctx := context.Background()
	ter = memstore.NewTerrain()
	ups, records, chunks, hours := historyUpdates(t, s)
	must[core.ApplyResult](t)(ter.Apply(ctx, ups))
	if records > 0 {
		if err := ter.Commit(ctx, core.WindowEnd(ups[len(ups)-1].T, W)); err != nil {
			t.Fatal(err)
		}
	}
	return ter, records, chunks, hours
}

// historyUpdates decodes a surface's z0 history into chunk updates, one per touched
// chunk and 60 s window (its content at the end of the window), sorted by time.
func historyUpdates(t testing.TB, s mmSurface) (all []core.ChunkUpdate, records, chunks int, hours float64) {
	seen := map[[3]int32]bool{}
	var first, last int64 = math.MaxInt64, math.MinInt64
	for _, a := range listAreas(t, filepath.Join(s.dir, "0")) {
		tile := core.TileCoord{Z: 0, X: a.ax, Y: a.ay}
		im := core.NewTileImage()
		curWin := int64(math.MinInt64)
		touched := map[uint8]int64{}
		var ups []core.ChunkUpdate
		flush := func() {
			for b, tt := range touched {
				px := im.Block(b)
				ups = append(ups, core.ChunkUpdate{DS: s.key, Chunk: tile.Chunk(b), T: core.TimeMs(tt), Pixels: &px})
			}
			clear(touched)
		}
		for _, c := range a.index.Changes {
			for _, r := range decodeMMC1(t, c[0], readGz(t, filepath.Join(a.dir, fmt.Sprintf("%d.c.gz", c[0])))) {
				ws := int64(core.WindowStart(core.TimeMs(r.t), W))
				if ws != curWin {
					flush()
					curWin = ws
				}
				applyMM(im, r)
				touched[r.block] = r.t
				seen[[3]int32{a.ax, a.ay, int32(r.block)}] = true
				first, last = min(first, r.t), max(last, r.t)
				records++
			}
		}
		flush()
		all = append(all, ups...)
	}
	slices.SortStableFunc(all, func(a, b core.ChunkUpdate) int { return cmp.Compare(a.T, b.T) })
	if records > 0 {
		hours = float64(last-first) / 3.6e6
	}
	return all, records, len(seen), hours
}

// TestIllisisLiveReplay replays the nauvis history as a live server would: per 60 s
// window, Apply, Commit and pyramid Update (timed). The result must equal an offline
// Rebuild of the same history, version for version.
func TestIllisisLiveReplay(t *testing.T) {
	root := testdataRoot(t)
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	var s mmSurface
	for _, c := range listSurfaces(t, root) {
		if c.key.Surface == "nauvis" {
			s = c
		}
	}
	ups, _, _, _ := historyUpdates(t, s)
	ter := memstore.NewTerrain()
	live := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
	var upd []time.Duration
	var stats BuildStats
	start := time.Now()
	for i := 0; i < len(ups); {
		ws := core.WindowStart(ups[i].T, W)
		j := i
		for j < len(ups) && core.WindowStart(ups[j].T, W) == ws {
			j++
		}
		must[core.ApplyResult](t)(ter.Apply(ctx, ups[i:j]))
		if err := ter.Commit(ctx, ws+W); err != nil {
			t.Fatal(err)
		}
		u0 := time.Now()
		st := must[BuildStats](t)(live.Update(ctx))
		upd = append(upd, time.Since(u0))
		stats.add(st)
		// Idle windows between changes: one more Update at the next window end lets
		// due levels fire on time, as a server committing every minute would.
		if j < len(ups) {
			next := core.WindowStart(ups[j].T, W)
			for h := ws + 2*W; h <= next; h += W {
				if err := ter.Commit(ctx, h); err != nil {
					t.Fatal(err)
				}
				u0 := time.Now()
				st := must[BuildStats](t)(live.Update(ctx))
				upd = append(upd, time.Since(u0))
				stats.add(st)
			}
		}
		i = j
	}
	must[BuildStats](t)(live.Flush(ctx))
	total := time.Since(start)
	var sum time.Duration
	for _, d := range upd {
		sum += d
	}
	t.Logf("live replay: %d Update calls, total %v (all %v incl. memstore apply/commit), mean %v, p99 %v, max %v; %+v",
		len(upd), sum.Round(time.Millisecond), total.Round(time.Millisecond), sum/time.Duration(len(upd)), pct(upd, 0.99), pct(upd, 1), stats)

	reb := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
	must[BuildStats](t)(reb.Rebuild(ctx, s.key))
	versions := 0
	for z := int8(-1); z >= core.MinZ; z-- {
		a := must[[]core.TileInfo](t)(live.ListTiles(ctx, s.key, z))
		b := must[[]core.TileInfo](t)(reb.ListTiles(ctx, s.key, z))
		if !slices.Equal(a, b) {
			t.Fatalf("z%d: live and rebuild tiles differ:\n%+v\n%+v", z, a, b)
		}
		for _, ti := range a {
			ha := must[[]core.VersionPoint](t)(live.History(ctx, s.key, ti.Tile, math.MinInt64, math.MaxInt64))
			hb := must[[]core.VersionPoint](t)(reb.History(ctx, s.key, ti.Tile, math.MinInt64, math.MaxInt64))
			if !slices.Equal(ha, hb) {
				t.Fatalf("%v: live and rebuild versions differ", ti.Tile)
			}
			versions += len(ha)
			im1 := must[*core.TileImage](t)(live.ReadVersion(ctx, s.key, ti.Tile, ti.Version))
			im2 := must[*core.TileImage](t)(reb.ReadVersion(ctx, s.key, ti.Tile, ti.Version))
			if !im1.Equal(im2) {
				t.Fatalf("%v: latest content differs", ti.Tile)
			}
		}
	}
	t.Logf("live replay and rebuild agree on %d coarse versions", versions)
}

// storedBytes sums payload, header (32 B) and index (24 B) bytes of every series under
// prefix, in total and per level.
func storedBytes(t *testing.T, ss core.SeriesStore, prefix string) (total, recs int64, perLevel map[int8]int64) {
	ctx := context.Background()
	perLevel = map[int8]int64{}
	for _, name := range must[[]string](t)(ss.List(ctx, prefix)) {
		sr := must[core.Series](t)(ss.Open(ctx, name, false))
		tile, _ := parseSeriesName(prefix, name)
		for i := uint64(0); i < sr.Len(); i++ {
			m := must[core.RecordMeta](t)(sr.Meta(i))
			perLevel[tile.Z] += int64(m.Len) + 32 + 24
			total += int64(m.Len) + 32 + 24
		}
		recs += int64(sr.Len())
	}
	return total, recs, perLevel
}

// TestIllisisHistory replays the full z0 history of every surface into 60 s windows,
// rebuilds the pyramid offline, and reports its size and read/encode timings. The final
// state must again match illisis z1..z4 bit for bit, and sampled coarse versions must
// equal the average of z0 at their effective time.
func TestIllisisHistory(t *testing.T) {
	root := testdataRoot(t)
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	var allBytes, allRecs int64
	var allChunkHours float64
	for _, s := range listSurfaces(t, root) {
		start := time.Now()
		ter, records, chunks, hours := loadHistory(t, s)
		if records == 0 {
			continue
		}
		loadDur := time.Since(start)
		ss := memstore.NewSeriesStore()
		p := newPyramid(t, ter, ss, Options{})
		start = time.Now()
		st := must[BuildStats](t)(p.Rebuild(ctx, s.key))
		buildDur := time.Since(start)
		stored, recs, perLevel := storedBytes(t, ss, s.key.ID()+"/pyramid/")
		chunkHours := float64(chunks) * hours
		t.Logf("%v: %d records, %d chunks, %.1f h; z0 load %v; rebuild %v: %+v", s.key, records, chunks, hours, loadDur.Round(time.Millisecond), buildDur.Round(time.Millisecond), st)
		var lv []string
		for z := int8(-1); z >= core.MinZ; z-- {
			if perLevel[z] > 0 {
				lv = append(lv, fmt.Sprintf("z%d=%d", z, perLevel[z]))
			}
		}
		t.Logf("    pyramid stored %d B in %d records (incl. 56 B/record header+index) = %.3f B/chunk-hour; per level: %s", stored, recs, float64(stored)/chunkHours, strings.Join(lv, " "))
		allBytes += stored
		allRecs += recs
		allChunkHours += chunkHours

		n := compareCoarse(t, p, s)
		t.Logf("    final state: %d coarse areas bit-identical to illisis z1-4", n)

		// Sampled historical versions against the reference average at their time.
		r := rand.New(rand.NewSource(1))
		checked := 0
		for z := int8(-1); z >= -4; z-- {
			for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, s.key, z)) {
				hist := must[[]core.VersionPoint](t)(p.History(ctx, s.key, ti.Tile, math.MinInt64, math.MaxInt64))
				for k := 0; k < 2; k++ {
					pt := hist[r.Intn(len(hist))]
					exp := expectAtDS(t, ter, s.key, ti.Tile, pt.T)
					got := must[*core.TileImage](t)(p.ReadVersion(ctx, s.key, ti.Tile, pt.Version))
					if !got.Equal(exp) {
						t.Fatalf("%v %v v%d at %v differs from the z0 average", s.key, ti.Tile, pt.Version, pt.T)
					}
					checked++
				}
			}
		}
		t.Logf("    %d sampled historical versions equal the z0 average at their time", checked)

		if s.key.Surface == "nauvis" {
			coldReads(t, p, ter, s.key)
		}
	}
	t.Logf("ALL: pyramid %d B in %d records = %.3f B/chunk-hour", allBytes, allRecs, float64(allBytes)/allChunkHours)
}

// TestIllisisTuning rebuilds every surface with several option sets and reports the
// stored size over the whole map, nauvis alone, and the cold historical read cost on
// nauvis. Set TSDB_TUNE=1 to run it.
func TestIllisisTuning(t *testing.T) {
	root := testdataRoot(t)
	if os.Getenv("TSDB_TUNE") == "" {
		t.Skip("TSDB_TUNE not set")
	}
	ctx := context.Background()
	type surf struct {
		s          mmSurface
		ter        *memstore.Terrain
		chunkHours float64
	}
	var surfs []surf
	for _, s := range listSurfaces(t, root) {
		ter, records, chunks, hours := loadHistory(t, s)
		if records > 0 {
			surfs = append(surfs, surf{s, ter, float64(chunks) * hours})
		}
	}
	iv := func(mins ...int) []core.TimeMs {
		var out []core.TimeMs
		for _, m := range mins {
			out = append(out, core.TimeMs(m)*core.Minute)
		}
		return out
	}
	for _, c := range []struct {
		name string
		opt  Options
	}{
		{"5/15/60 r32 c512", Options{Intervals: iv(5, 15, 60), SnapshotRatio: 32, MaxChain: 512}},
		{"15/60/240 r16 c256", Options{Intervals: iv(15, 60, 240), SnapshotRatio: 16, MaxChain: 256}},
		{"15/60/240 r32 c512", Options{Intervals: iv(15, 60, 240), SnapshotRatio: 32, MaxChain: 512}},
		{"15/60/240 r64 c1024", Options{Intervals: iv(15, 60, 240), SnapshotRatio: 64, MaxChain: 1024}},
		{"30/60/240 r32 c512", Options{Intervals: iv(30, 60, 240), SnapshotRatio: 32, MaxChain: 512}},
		{"15/60/240/1440 r32 c512", Options{Intervals: iv(15, 60, 240, 240, 1440), SnapshotRatio: 32, MaxChain: 512}},
		{"15/60/240/1440 r64 c1024", Options{Intervals: iv(15, 60, 240, 240, 1440), SnapshotRatio: 64, MaxChain: 1024}},
		{"15/60/480 r64 c512", Options{Intervals: iv(15, 60, 480), SnapshotRatio: 64, MaxChain: 512}}, // default
		{"30/60/240 r64 c512", Options{Intervals: iv(30, 60, 240), SnapshotRatio: 64, MaxChain: 512}},
	} {
		var all, nauvis int64
		var allCH, nauvisCH float64
		var cold []time.Duration
		var dur time.Duration
		for _, sf := range surfs {
			ss := memstore.NewSeriesStore()
			p := newPyramid(t, sf.ter, ss, c.opt)
			start := time.Now()
			must[BuildStats](t)(p.Rebuild(ctx, sf.s.key))
			dur += time.Since(start)
			stored, _, _ := storedBytes(t, ss, sf.s.key.ID()+"/pyramid/")
			all += stored
			allCH += sf.chunkHours
			if sf.s.key.Surface == "nauvis" {
				nauvis, nauvisCH = stored, sf.chunkHours
				cold = coldHistorical(t, p, sf.s.key, 200)
			}
			p.Close()
		}
		t.Logf("%-24s all %8d B = %.3f B/chunk-hour; nauvis %8d B = %.3f; build %v; nauvis cold hist read p50 %v p95 %v",
			c.name, all, float64(all)/allCH, nauvis, float64(nauvis)/nauvisCH, dur.Round(time.Millisecond), pct(cold, 0.5), pct(cold, 0.95))
	}
}

// expectAtDS computes one coarse tile at T from z0 with the reference core.Downsample.
func expectAtDS(t testing.TB, ter core.TerrainStore, key core.DatastoreKey, tile core.TileCoord, T core.TimeMs) *core.TileImage {
	ctx := context.Background()
	if tile.Z == 0 {
		st, err := ter.VersionAt(ctx, key, tile, T)
		if err != nil {
			return nil
		}
		return must[*core.TileImage](t)(ter.ReadVersion(ctx, key, tile, st.Version))
	}
	var kids [4]*core.TileImage
	for i, c := range tile.Children() {
		kids[i] = expectAtDS(t, ter, key, c, T)
	}
	return core.Downsample(kids)
}

// pct returns the q-quantile of d (sorted in place).
func pct(d []time.Duration, q float64) time.Duration {
	slices.Sort(d)
	return d[min(len(d)-1, int(q*float64(len(d))))]
}

// timeOp returns the mean duration of fn over reps repetitions (the Windows timer has
// about 0.5 ms resolution).
func timeOp(reps int, fn func()) time.Duration {
	start := time.Now()
	for i := 0; i < reps; i++ {
		fn()
	}
	return time.Since(start) / time.Duration(reps)
}

// coldHistorical returns the mean cold read time of n random (tile, version) pairs at
// z-1..-5.
func coldHistorical(t *testing.T, p *Pyramid, key core.DatastoreKey, n int) []time.Duration {
	ctx := context.Background()
	r := rand.New(rand.NewSource(2))
	var out []time.Duration
	for i := 0; i < n; i++ {
		z := int8(-1 - r.Intn(5))
		tiles := must[[]core.TileInfo](t)(p.ListTiles(ctx, key, z))
		ti := tiles[r.Intn(len(tiles))]
		v := core.Version(1 + r.Intn(int(ti.Version)))
		out = append(out, timeOp(10, func() {
			p.PurgeCache()
			must[*core.TileImage](t)(p.ReadVersion(ctx, key, ti.Tile, v))
		}))
	}
	return out
}

// coldReads measures latest coarse reads with an empty decoded cache, and encoded
// response times for real tiles. Each figure is the mean over repetitions on one tile;
// percentiles are over tiles.
func coldReads(t *testing.T, p *Pyramid, ter *memstore.Terrain, key core.DatastoreKey) {
	ctx := context.Background()
	for _, z := range []int8{-1, -3, -5, -8} {
		var ds []time.Duration
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, key, z)) {
			ds = append(ds, timeOp(20, func() {
				p.PurgeCache()
				must[*core.TileImage](t)(p.ReadVersion(ctx, key, ti.Tile, ti.Version))
			}))
		}
		t.Logf("    cold latest read z%d: tiles=%d p50 %v max %v", z, len(ds), pct(ds, 0.5), pct(ds, 1))
	}
	hd := coldHistorical(t, p, key, 200)
	t.Logf("    cold historical read z-1..-5: n=%d p50 %v p95 %v max %v", len(hd), pct(hd, 0.5), pct(hd, 0.95), pct(hd, 1))

	// TSV3 encode + compression of the latest z0 and z-1 images.
	pal := must[core.Palette](t)(ter.Palette(ctx, key))
	lut := wire.NewLUT(pal)
	comp, err := tileenc.NewCompressor(tileenc.CompressorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer comp.Close()
	type sample struct {
		im  *core.TileImage
		lut wire.LUT
		z   int8
	}
	samples := map[string][]sample{}
	for _, ti := range must[[]core.TileInfo](t)(ter.ListTiles(ctx, key, 0)) {
		samples["z0"] = append(samples["z0"], sample{must[*core.TileImage](t)(ter.ReadVersion(ctx, key, ti.Tile, ti.Version)), lut, 0})
	}
	for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, key, -1)) {
		samples["z-1"] = append(samples["z-1"], sample{must[*core.TileImage](t)(p.ReadVersion(ctx, key, ti.Tile, ti.Version)), nil, -1})
	}
	fast, err := tileenc.NewCompressor(tileenc.CompressorOptions{ZstdLevel: zstd.SpeedFastest})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	for _, cfg := range []struct {
		name string
		c    *tileenc.Compressor
		enc  tileenc.Encoding
	}{{"identity", comp, tileenc.Identity}, {"gzip-1", comp, tileenc.Gzip}, {"zstd-default", comp, tileenc.Zstd}, {"zstd-fastest", fast, tileenc.Zstd}} {
		for _, kind := range []string{"z0", "z-1"} {
			var ds []time.Duration
			var size int
			for _, sm := range samples[kind] {
				var out []byte
				ds = append(ds, timeOp(10, func() {
					raw, err := tileenc.EncodeTile(wire.TileHeader{Z: sm.z}, sm.im, sm.lut)
					if err != nil {
						t.Fatal(err)
					}
					if out, err = cfg.c.Compress(cfg.enc, raw); err != nil {
						t.Fatal(err)
					}
				}))
				size += len(out)
			}
			t.Logf("    encode+compress %s %s: tiles=%d p50 %v p95 %v max %v, avg %d B", kind, cfg.name, len(ds), pct(ds, 0.5), pct(ds, 0.95), pct(ds, 1), size/len(ds))
		}
	}

	// Renderer: miss (cold pyramid read + encode + zstd) and hit, coarse tiles.
	rd, err := tileenc.NewRenderer(p, ter, tileenc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var miss, hit []time.Duration
	for z := int8(-1); z >= -5; z-- {
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, key, z)) {
			miss = append(miss, timeOp(10, func() {
				p.PurgeCache()
				rd.PurgeCache()
				if _, err := rd.Tile(ctx, key, ti.Tile, ti.Version, tileenc.Zstd); err != nil {
					t.Fatal(err)
				}
			}))
			hit = append(hit, timeOp(10000, func() {
				if b, err := rd.Tile(ctx, key, ti.Tile, ti.Version, tileenc.Zstd); err != nil || len(b.Data) == 0 {
					t.Fatal(err)
				}
			}))
		}
	}
	t.Logf("    renderer coarse tile zstd: miss tiles=%d p50 %v p95 %v max %v; hit p50 %v max %v",
		len(miss), pct(miss, 0.5), pct(miss, 0.95), pct(miss, 1), pct(hit, 0.5), pct(hit, 1))
}
