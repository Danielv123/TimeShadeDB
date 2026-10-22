package pyramid

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/memstore"
	"timeshadedb/internal/core/wire"
	"timeshadedb/internal/tileenc"
)

func BenchmarkDownsample(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	var kids [4]*core.TileImage
	for i := range kids {
		kids[i] = core.NewTileImage()
		for j := range kids[i].Pix {
			kids[i].Pix[j] = uint16(r.Intn(65536))
		}
	}
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			downsample(kids)
		}
	})
	b.Run("core", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			core.Downsample(kids)
		}
	})
}

// realFixture is the nauvis history of the illisis export, rebuilt once per run.
type realFixture struct {
	ter *memstore.Terrain
	p   *Pyramid
	key core.DatastoreKey
}

var (
	realOnce sync.Once
	real     *realFixture
)

func loadReal(b *testing.B) *realFixture {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		b.Skip("TSDB_TESTDATA not set (illisis export root)")
	}
	if _, err := os.Stat(filepath.Join(root, "maps.json.gz")); err != nil {
		b.Skip(err)
	}
	// A failure inside Do fails this benchmark (b.Fatal ends its goroutine); later
	// benchmarks then skip because real stays nil.
	realOnce.Do(func() {
		for _, s := range listSurfaces(b, root) {
			if s.key.Surface != "nauvis" {
				continue
			}
			ter, _, _, _ := loadHistory(b, s)
			p, err := New(context.Background(), ter, memstore.NewSeriesStore(), Options{})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := p.Rebuild(context.Background(), s.key); err != nil {
				b.Fatal(err)
			}
			real = &realFixture{ter: ter, p: p, key: s.key}
		}
	})
	if real == nil {
		b.Skip("no nauvis surface in TSDB_TESTDATA")
	}
	return real
}

// largest returns the tile at level z whose latest version compresses worst.
func (f *realFixture) largest(b testing.TB, z int8) (core.TileInfo, *core.TileImage) {
	ctx := context.Background()
	comp, _ := tileenc.NewCompressor(tileenc.CompressorOptions{})
	defer comp.Close()
	var best core.TileInfo
	var bestIm *core.TileImage
	bestSize := -1
	for _, ti := range must[[]core.TileInfo](b)(f.p.ListTiles(ctx, f.key, z)) {
		im := must[*core.TileImage](b)(f.p.ReadVersion(ctx, f.key, ti.Tile, ti.Version))
		raw, _ := tileenc.EncodeTile(wire.TileHeader{}, im, nil)
		if out, _ := comp.Compress(tileenc.Zstd, raw); len(out) > bestSize {
			best, bestIm, bestSize = ti, im, len(out)
		}
	}
	return best, bestIm
}

// BenchmarkRealEncode: TSV3 encode + compression of the worst-compressing latest nauvis
// tile at z0 and z-1 (PLAN.md WP3: encoded tile <= 2 ms).
func BenchmarkRealEncode(b *testing.B) {
	f := loadReal(b)
	ctx := context.Background()
	pal := must[core.Palette](b)(f.ter.Palette(ctx, f.key))
	lut := wire.NewLUT(pal)
	_, z0 := f.largest(b, 0)
	_, zm1 := f.largest(b, -1)
	def, _ := tileenc.NewCompressor(tileenc.CompressorOptions{})
	fast, _ := tileenc.NewCompressor(tileenc.CompressorOptions{ZstdLevel: zstd.SpeedFastest})
	for _, c := range []struct {
		name string
		im   *core.TileImage
		lut  wire.LUT
		comp *tileenc.Compressor
		enc  tileenc.Encoding
	}{
		{"z0/identity", z0, lut, def, tileenc.Identity},
		{"z0/gzip", z0, lut, def, tileenc.Gzip},
		{"z0/zstd", z0, lut, def, tileenc.Zstd},
		{"z0/zstd-fastest", z0, lut, fast, tileenc.Zstd},
		{"z-1/identity", zm1, nil, def, tileenc.Identity},
		{"z-1/gzip", zm1, nil, def, tileenc.Gzip},
		{"z-1/zstd", zm1, nil, def, tileenc.Zstd},
		{"z-1/zstd-fastest", zm1, nil, fast, tileenc.Zstd},
	} {
		b.Run(c.name, func(b *testing.B) {
			var n int
			for i := 0; i < b.N; i++ {
				raw, err := tileenc.EncodeTile(wire.TileHeader{}, c.im, c.lut)
				if err != nil {
					b.Fatal(err)
				}
				out, err := c.comp.Compress(c.enc, raw)
				if err != nil {
					b.Fatal(err)
				}
				n = len(out)
			}
			b.ReportMetric(float64(n), "bytes")
		})
	}
}

// BenchmarkRealColdRead: latest coarse tile read with an empty decoded cache (PLAN.md
// WP3: z=-5 tile from latest <= 20 ms cold), and a random historical version.
func BenchmarkRealColdRead(b *testing.B) {
	f := loadReal(b)
	ctx := context.Background()
	for _, z := range []int8{-1, -5} {
		ti, _ := f.largest(b, z)
		b.Run("latest/z"+itoa(z), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f.p.PurgeCache()
				must[*core.TileImage](b)(f.p.ReadVersion(ctx, f.key, ti.Tile, ti.Version))
			}
		})
	}
	ti, _ := f.largest(b, -1)
	r := rand.New(rand.NewSource(1))
	b.Run("historical/z-1", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			v := core.Version(1 + r.Intn(int(ti.Version)))
			f.p.PurgeCache()
			must[*core.TileImage](b)(f.p.ReadVersion(ctx, f.key, ti.Tile, v))
		}
	})
}

// BenchmarkRealRenderer: encoded response through tileenc.Renderer, cache hit (PLAN.md
// WP3: cache hit <= 50 us) and miss (cold pyramid read + encode + zstd), z-1.
func BenchmarkRealRenderer(b *testing.B) {
	f := loadReal(b)
	ctx := context.Background()
	rd, _ := tileenc.NewRenderer(f.p, f.ter, tileenc.Options{})
	defer rd.Close()
	ti, _ := f.largest(b, -1)
	b.Run("hit", func(b *testing.B) {
		rd.Tile(ctx, f.key, ti.Tile, ti.Version, tileenc.Zstd)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := rd.Tile(ctx, f.key, ti.Tile, ti.Version, tileenc.Zstd); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("hit-parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := rd.Tile(ctx, f.key, ti.Tile, ti.Version, tileenc.Zstd); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.p.PurgeCache()
			rd.PurgeCache()
			if _, err := rd.Tile(ctx, f.key, ti.Tile, ti.Version, tileenc.Zstd); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkRealUpdateIdle: Update when nothing changed (called after every commit).
func BenchmarkRealUpdateIdle(b *testing.B) {
	f := loadReal(b)
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		if _, err := f.p.Update(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func itoa(z int8) string {
	if z < 0 {
		return "-" + string(rune('0'-z))
	}
	return string(rune('0' + z))
}

// TestIllisisEncodeLatency times single TSV3 encode + compression calls on the
// worst-compressing latest nauvis tiles at z0 and z-1 and reports per-call percentiles
// (PLAN.md WP3: encoded tile <= 2 ms). It needs TSDB_TESTDATA and TSDB_TUNE=1.
func TestIllisisEncodeLatency(t *testing.T) {
	root := testdataRoot(t)
	if os.Getenv("TSDB_TUNE") == "" {
		t.Skip("TSDB_TUNE not set")
	}
	ctx := context.Background()
	var s mmSurface
	for _, c := range listSurfaces(t, root) {
		if c.key.Surface == "nauvis" {
			s = c
		}
	}
	ter, _, _, _ := loadHistory(t, s)
	p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
	must[BuildStats](t)(p.Rebuild(ctx, s.key))
	f := &realFixture{ter: ter, p: p, key: s.key}
	pal := must[core.Palette](t)(ter.Palette(ctx, s.key))
	lut := wire.NewLUT(pal)
	_, z0 := f.largest(t, 0)
	_, zm1 := f.largest(t, -1)
	def, _ := tileenc.NewCompressor(tileenc.CompressorOptions{})
	defer def.Close()
	fast, _ := tileenc.NewCompressor(tileenc.CompressorOptions{ZstdLevel: zstd.SpeedFastest})
	defer fast.Close()
	for _, c := range []struct {
		name string
		im   *core.TileImage
		lut  wire.LUT
		comp *tileenc.Compressor
		enc  tileenc.Encoding
	}{
		{"z0 gzip-1", z0, lut, def, tileenc.Gzip},
		{"z0 zstd-default", z0, lut, def, tileenc.Zstd},
		{"z0 zstd-fastest", z0, lut, fast, tileenc.Zstd},
		{"z-1 gzip-1", zm1, nil, def, tileenc.Gzip},
		{"z-1 zstd-default", zm1, nil, def, tileenc.Zstd},
		{"z-1 zstd-fastest", zm1, nil, fast, tileenc.Zstd},
	} {
		var d []time.Duration
		var n int
		for i := 0; i < 520; i++ {
			start := hrNow()
			raw, err := tileenc.EncodeTile(wire.TileHeader{}, c.im, c.lut)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.comp.Compress(c.enc, raw)
			if err != nil {
				t.Fatal(err)
			}
			if i >= 20 { // warm-up
				d = append(d, hrSince(start))
			}
			n = len(out)
		}
		t.Logf("%-18s %6d B  p50 %v  p95 %v  p99 %v  max %v", c.name, n, pct(d, 0.5), pct(d, 0.95), pct(d, 0.99), slices.Max(d))
	}
}
