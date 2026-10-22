package tileenc

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"math/rand"
	"runtime"
	"sync"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
	"timeshadedb/internal/core/memstore"
	"timeshadedb/internal/core/wire"
)

// randomImage returns a tile using the first n colours of a palette, with some
// uncharted pixels, plus that palette.
func randomImage(r *rand.Rand, n int) (*core.TileImage, core.Palette) {
	pal := core.Palette{Colors: []uint16{0}}
	seen := map[uint16]bool{0: true}
	for len(pal.Colors) < n {
		c := uint16(r.Intn(65535) + 1)
		if !seen[c] {
			seen[c] = true
			pal.Colors = append(pal.Colors, c)
		}
	}
	im := core.NewTileImage()
	for i := range im.Pix {
		if r.Intn(5) > 0 {
			im.Pix[i] = pal.Colors[r.Intn(len(pal.Colors))]
		}
	}
	return im, pal
}

func TestEncodeTileMatchesWire(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 2, 200, 256, 257, 1000} {
		im, pal := randomImage(r, n)
		lut := wire.NewLUT(pal)
		h := wire.TileHeader{Z: 0, X: -3, Y: 7, Version: 42}
		want, err := wire.EncodeTile(h, im, lut)
		if err != nil {
			t.Fatal(err)
		}
		got, err := EncodeTile(h, im, lut)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("palette %d: paletted encoding differs from wire.EncodeTile", n)
		}
		hc := wire.TileHeader{Z: -2, X: 1, Y: -1, Version: 3}
		want, _ = wire.EncodeTile(hc, im, nil)
		got, _ = EncodeTile(hc, im, nil)
		if !bytes.Equal(got, want) {
			t.Fatal("rgb565 encoding differs from wire.EncodeTile")
		}
		// A colour outside the palette.
		im.Pix[1234] = 0xFFFF
		if lut[0xFFFF] < 0 {
			if _, err := EncodeTile(h, im, lut); !errors.Is(err, ErrColour) {
				t.Fatalf("missing colour err = %v", err)
			}
		}
	}
}

func TestEncodeDeltaMatchesWire(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	_, pal := randomImage(r, 300)
	lut := wire.NewLUT(pal)
	var deltas []core.ChunkDelta
	for _, b := range []uint8{0, 17, 200, 255} {
		var d core.ChunkDelta
		d.Block = b
		for i := range d.Pixels {
			d.Pixels[i] = pal.Colors[r.Intn(len(pal.Colors))]
		}
		deltas = append(deltas, d)
	}
	h := wire.DeltaHeader{X: 1, Y: 2, From: 3, To: 5}
	want, err := wire.EncodeTileDelta(h, deltas, lut)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EncodeTileDelta(h, deltas, lut)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("delta differs: %v", err)
	}
	deltas[1].Pixels[5] = 0xFFFF
	if lut[0xFFFF] < 0 {
		if _, err := EncodeTileDelta(h, deltas, lut); !errors.Is(err, ErrColour) {
			t.Fatalf("missing colour err = %v", err)
		}
	}
}

func TestNegotiate(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Encoding
	}{
		{"", Identity},
		{"gzip, deflate, br, zstd", Zstd},
		{"gzip, deflate, br", Gzip},
		{"zstd;q=0, gzip", Gzip},
		{"gzip;q=0", Identity},
		{"*", Zstd},
		{"*;q=0", Identity},
		{"identity", Identity},
		{"ZSTD", Zstd},
		{"gzip;q=0.5, zstd;q=0.1", Zstd},
		{"x-gzip", Gzip},
		{"zstd;q=0, *", Gzip},
	} {
		if got := Negotiate(c.in); got != c.want {
			t.Errorf("Negotiate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func decompress(t *testing.T, b Body) []byte {
	t.Helper()
	switch b.Encoding {
	case Identity:
		return b.Data
	case Zstd:
		d, _ := zstd.NewReader(nil)
		defer d.Close()
		out, err := d.DecodeAll(b.Data, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	case Gzip:
		zr, err := gzip.NewReader(bytes.NewReader(b.Data))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Fatalf("encoding %v", b.Encoding)
	return nil
}

func TestCompressorRoundTrip(t *testing.T) {
	c, err := NewCompressor(CompressorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := bytes.Repeat([]byte("timeshade v3 "), 10000)
	for _, e := range []Encoding{Identity, Gzip, Zstd} {
		out, err := c.Compress(e, raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := decompress(t, Body{Data: out, Encoding: e}); !bytes.Equal(got, raw) {
			t.Fatalf("%v round trip failed", e)
		}
		if e != Identity && len(out) >= len(raw)/10 {
			t.Fatalf("%v did not compress: %d", e, len(out))
		}
	}
}

func TestLRU(t *testing.T) {
	c := NewLRU[int, []byte](1000, 1, func(b []byte) int64 { return int64(len(b)) })
	for i := 0; i < 10; i++ {
		c.Put(i, make([]byte, 100))
	}
	if st := c.Stats(); st.Bytes != 1000 || st.Entries != 10 {
		t.Fatalf("stats = %+v", st)
	}
	c.Get(0) // 0 becomes most recent; 1 is now the oldest
	c.Put(10, make([]byte, 100))
	if _, ok := c.Get(1); ok {
		t.Fatal("oldest entry not evicted")
	}
	if _, ok := c.Get(0); !ok {
		t.Fatal("recently used entry evicted")
	}
	c.Put(11, make([]byte, 2000)) // larger than the budget: not cached
	if _, ok := c.Get(11); ok {
		t.Fatal("oversized entry cached")
	}
	c.Put(0, make([]byte, 50)) // replace
	if st := c.Stats(); st.Bytes != 950 || st.Entries != 10 || st.Evictions != 1 {
		t.Fatalf("after replace stats = %+v", st)
	}
	c.Delete(0)
	c.Purge()
	if st := c.Stats(); st.Bytes != 0 || st.Entries != 0 {
		t.Fatalf("after purge stats = %+v", st)
	}

	// Concurrent use stays within budget.
	cc := NewLRU[int, []byte](64<<10, 8, func(b []byte) int64 { return int64(len(b)) })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 20000; i++ {
				k := r.Intn(2000)
				if _, ok := cc.Get(k); !ok {
					cc.Put(k, make([]byte, 100+r.Intn(400)))
				}
			}
		}(g)
	}
	wg.Wait()
	if st := cc.Stats(); st.Bytes > st.MaxBytes || st.Bytes <= 0 {
		t.Fatalf("concurrent stats = %+v", st)
	}
}

// fixture builds a memstore terrain with two committed versions of tile (0,0), a
// second tile, and an open-window update, plus a memstore pyramid over it.
func fixture(t testing.TB) (*memstore.Terrain, *memstore.Pyramid) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	ds := coretest.DS
	mk := func(seed int64) *core.ChunkPixels {
		r := rand.New(rand.NewSource(seed))
		var px core.ChunkPixels
		for i := range px {
			px[i] = uint16(r.Intn(40)) * 1601
		}
		return &px
	}
	if _, err := ter.Apply(ctx, []core.ChunkUpdate{
		{DS: ds, Chunk: core.ChunkCoord{X: -8, Y: -8}, T: 1000, Pixels: mk(1)},
		{DS: ds, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: 2000, Pixels: mk(2)},
		{DS: ds, Chunk: core.ChunkCoord{X: 8, Y: -8}, T: 3000, Pixels: mk(3)},
		{DS: ds, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: 61_000, Pixels: mk(4)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ter.Commit(ctx, 120_000); err != nil {
		t.Fatal(err)
	}
	if _, err := ter.Apply(ctx, []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{X: 1, Y: 1}, T: 130_000, Pixels: coretest.Fill(0xABCD)}}); err != nil {
		t.Fatal(err)
	}
	return ter, memstore.NewPyramid(ter)
}

func TestRenderer(t *testing.T) {
	ctx := context.Background()
	ter, pyr := fixture(t)
	r, err := NewRenderer(pyr, ter, Options{CacheBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ds := coretest.DS
	tile := core.TileCoord{Z: 0, X: 0, Y: 0}

	for _, enc := range []Encoding{Identity, Gzip, Zstd} {
		for v := core.Version(0); v <= 2; v++ {
			b, err := r.Tile(ctx, ds, tile, v, enc)
			if err != nil {
				t.Fatal(err)
			}
			if b.Encoding != enc {
				t.Fatalf("encoding %v, want %v", b.Encoding, enc)
			}
			pal, _ := ter.Palette(ctx, ds)
			h, im, err := wire.DecodeTile(decompress(t, b), &pal)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := ter.ReadVersion(ctx, ds, tile, v)
			if h.Version != v || h.Format != wire.FormatPal8 || !im.Equal(want) {
				t.Fatalf("v%d %v: decoded tile differs (header %+v)", v, enc, h)
			}
		}
	}
	before := r.CacheStats()
	if _, err := r.Tile(ctx, ds, tile, 2, Zstd); err != nil {
		t.Fatal(err)
	}
	if after := r.CacheStats(); after.Hits != before.Hits+1 {
		t.Fatalf("second request was not a cache hit: %+v -> %+v", before, after)
	}

	// Coarse tile: RGB565.
	ct := core.TileCoord{Z: -1, X: 0, Y: 0}
	st, _ := pyr.VersionAt(ctx, ds, ct, 1<<40)
	b, err := r.Tile(ctx, ds, ct, st.Version, Gzip)
	if err != nil {
		t.Fatal(err)
	}
	h, im, err := wire.DecodeTile(decompress(t, b), nil)
	want, _ := pyr.ReadVersion(ctx, ds, ct, st.Version)
	if err != nil || h.Format != wire.FormatRGB565 || h.Z != -1 || !im.Equal(want) {
		t.Fatalf("coarse tile: %v %+v", err, h)
	}

	// Errors are reported and not cached.
	if _, err := r.Tile(ctx, ds, tile, 3, Zstd); !errors.Is(err, core.ErrNoVersion) {
		t.Fatalf("version above latest err = %v", err)
	}
	if _, err := r.Tile(ctx, ds, core.TileCoord{Z: 0, X: 99999, Y: 0}, 1, Zstd); !errors.Is(err, core.ErrBadTile) {
		t.Fatalf("bad tile err = %v", err)
	}
	if _, err := r.Tile(ctx, ds, core.TileCoord{Z: 0, X: 30, Y: 30}, 1, Zstd); !errors.Is(err, core.ErrNoTile) {
		t.Fatalf("empty tile err = %v", err)
	}
	if _, err := r.Tile(ctx, core.DatastoreKey{Map: "x", Surface: "y", Force: "z"}, tile, 1, Zstd); !errors.Is(err, core.ErrUnknownDatastore) {
		t.Fatalf("unknown datastore err = %v", err)
	}

	// Delta.
	db, err := r.Delta(ctx, ds, tile, 1, 2, Zstd)
	if err != nil {
		t.Fatal(err)
	}
	pal, _ := ter.Palette(ctx, ds)
	dh, deltas, err := wire.DecodeTileDelta(decompress(t, db), &pal)
	wantD, _ := ter.ChangedChunks(ctx, ds, tile, 1, 2)
	if err != nil || dh.From != 1 || dh.To != 2 || len(deltas) != len(wantD) || deltas[0] != wantD[0] {
		t.Fatalf("delta: %v %+v", err, dh)
	}
	if _, err := r.Delta(ctx, ds, tile, 2, 2, Zstd); !errors.Is(err, core.ErrNoVersion) {
		t.Fatalf("empty delta err = %v", err)
	}

	// Palette prefix.
	pb, err := r.Palette(ctx, ds, 3, Identity)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := wire.DecodePalette(pb.Data)
	if err != nil || len(p3.Colors) != 3 || p3.Colors[2] != pal.Colors[2] {
		t.Fatalf("palette prefix: %v %v", err, p3)
	}
	if _, err := r.Palette(ctx, ds, uint32(len(pal.Colors)+1), Identity); !errors.Is(err, core.ErrNoVersion) {
		t.Fatalf("palette beyond current err = %v", err)
	}

	// Live tile: flag set, content includes the open update.
	lb, lst, err := r.LiveTile(ctx, ds, tile, 1<<40, Identity)
	if err != nil {
		t.Fatal(err)
	}
	pal, _ = ter.Palette(ctx, ds)
	lh, lim, err := wire.DecodeTile(lb.Data, &pal)
	wantLive, _, _ := ter.ReadTile(ctx, ds, tile, 1<<40, true)
	if err != nil || !lh.Live || lh.Version != 2 || !lst.Live || !lim.Equal(wantLive) {
		t.Fatalf("live tile: %v %+v %+v", err, lh, lst)
	}

	// Coarse live tiles need a LiveSource; the reference oracle is not one.
	if _, _, err := r.LiveTile(ctx, ds, core.TileCoord{Z: -1}, 1<<40, Identity); !errors.Is(err, core.ErrBadTile) {
		t.Fatalf("coarse live tile without a live source err = %v", err)
	}
	lr, err := NewRenderer(liveOracle{pyr}, ter, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	cl, cst, err := lr.LiveTile(ctx, ds, core.TileCoord{Z: -1}, 1<<40, Zstd)
	if err != nil {
		t.Fatal(err)
	}
	ch, cim, err := wire.DecodeTile(decompress(t, cl), nil)
	wantC, _ := pyr.ReadVersion(ctx, ds, core.TileCoord{Z: -1}, cst.Version)
	if err != nil || !ch.Live || ch.Z != -1 || ch.Version != cst.Version || !cst.Live || !cim.Equal(wantC) {
		t.Fatalf("coarse live tile: %v %+v %+v", err, ch, cst)
	}

	// PNG decodes to the same colours (alpha 0 for uncharted).
	pngb, err := r.PNG(ctx, ds, tile, 2)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(pngb.Data))
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := ter.ReadVersion(ctx, ds, tile, 2)
	for _, i := range []int{0, 1, 31 * 513, 256*512 + 256, 300*512 + 300} {
		cr, cg, cb, ca := img.At(i%512, i/512).RGBA()
		er, eg, eb := core.RGB565ToRGB(v2.Pix[i])
		if v2.Pix[i] == 0 {
			if ca != 0 {
				t.Fatalf("pixel %d: uncharted not transparent", i)
			}
			continue
		}
		if uint8(cr>>8) != er || uint8(cg>>8) != eg || uint8(cb>>8) != eb || ca != 0xffff {
			t.Fatalf("pixel %d: png colour differs", i)
		}
	}
	cpng, err := r.PNG(ctx, ds, ct, st.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(cpng.Data)); err != nil {
		t.Fatal(err)
	}

	// Custom bodies share the cache.
	calls := 0
	for i := 0; i < 3; i++ {
		cb, err := r.Cached(ctx, "timeline/x", Gzip, func(context.Context) ([]byte, error) { calls++; return []byte(`{"a":1}`), nil })
		if err != nil || string(decompress(t, cb)) != `{"a":1}` {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("custom body built %d times", calls)
	}
}

// TestRendererPaletteRefresh: a tile using colours added after the renderer cached the
// palette is still encoded (the renderer refreshes its LUT once).
func TestRendererPaletteRefresh(t *testing.T) {
	ctx := context.Background()
	ter, pyr := fixture(t)
	r, _ := NewRenderer(pyr, ter, Options{})
	defer r.Close()
	ds := coretest.DS
	if _, err := r.Tile(ctx, ds, core.TileCoord{Z: 0, X: 0, Y: 0}, 1, Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := ter.Apply(ctx, []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{X: 2, Y: 2}, T: 200_000, Pixels: coretest.Fill(0x1234)}}); err != nil {
		t.Fatal(err)
	}
	if err := ter.Commit(ctx, 300_000); err != nil {
		t.Fatal(err)
	}
	b, err := r.Tile(ctx, ds, core.TileCoord{Z: 0, X: 0, Y: 0}, 4, Identity)
	if err != nil {
		t.Fatal(err)
	}
	pal, _ := ter.Palette(ctx, ds)
	_, im, err := wire.DecodeTile(b.Data, &pal)
	if err != nil || im.Pix[core.TilePixelIndex(10*16+10, 0)] != 0x1234 {
		t.Fatalf("refreshed palette tile: %v", err)
	}
}

// TestRendererSingleFlight: concurrent requests for one uncached body build it once.
func TestRendererSingleFlight(t *testing.T) {
	ctx := context.Background()
	r, _ := NewRenderer(nil, nil, Options{})
	defer r.Close()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := r.Cached(ctx, "slow", Identity, func(context.Context) ([]byte, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release
				return []byte("x"), nil
			})
			if err != nil || string(b.Data) != "x" {
				t.Error(err)
			}
		}()
	}
	for {
		r.flightMu.Lock()
		n := len(r.flights)
		r.flightMu.Unlock()
		if n == 1 {
			break
		}
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("built %d times", calls)
	}
}

// TestRendererHostile: a million random out-of-grid, empty or unknown-datastore tile
// requests leave no lasting allocations (the Hostile requests targets in bench/targets.json).
func TestRendererHostile(t *testing.T) {
	ctx := context.Background()
	ter, pyr := fixture(t)
	r, _ := NewRenderer(pyr, ter, Options{})
	defer r.Close()
	ds := coretest.DS
	bad := core.DatastoreKey{Map: "nope", Surface: "nauvis", Force: "player"}
	rng := rand.New(rand.NewSource(3))
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	n := 1_000_000
	if testing.Short() {
		n = 100_000
	}
	for i := 0; i < n; i++ {
		tile := core.TileCoord{Z: int8(-rng.Intn(12)), X: int32(rng.Intn(1<<20) - 1<<19), Y: int32(rng.Intn(1<<20) - 1<<19)}
		key := ds
		if i%2 == 0 {
			key = bad
		}
		if i%3 == 0 {
			tile = core.TileCoord{Z: 0, X: int32(rng.Intn(2000) + 10), Y: int32(rng.Intn(2000) + 10)}
		}
		if _, err := r.Tile(ctx, key, tile, core.Version(rng.Intn(5)), Zstd); err == nil {
			t.Fatalf("request %v %v succeeded", key, tile)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	grow := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	t.Logf("%d hostile requests: heap %+d bytes, cache entries %d", n, grow, r.CacheStats().Entries)
	if grow > 2<<20 {
		t.Fatalf("heap grew by %d bytes", grow)
	}
	if r.CacheStats().Entries != 0 || len(r.flights) != 0 {
		t.Fatal("errors left state behind")
	}
}

// liveOracle adds a LiveSource to the reference pyramid: its live content is the
// latest version, flagged live.
type liveOracle struct{ core.TileSource }

func (o liveOracle) ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error) {
	st, err := o.VersionAt(ctx, ds, tile, t)
	if err != nil {
		return nil, st, err
	}
	im, err := o.ReadVersion(ctx, ds, tile, st.Version)
	st.Live, st.LiveSeq = live, 1
	return im, st, err
}
