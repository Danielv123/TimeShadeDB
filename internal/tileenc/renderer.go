package tileenc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/wire"
)

// Terrain is what the Renderer needs from the z0 store for palettes, live tiles and
// deltas. core.TerrainStore satisfies it.
type Terrain interface {
	Palette(ctx context.Context, ds core.DatastoreKey) (core.Palette, error)
	ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error)
	ChangedChunks(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, from, to core.Version) ([]core.ChunkDelta, error)
}

// Options configures a Renderer. Zero values pick the defaults.
type Options struct {
	// CacheBytes bounds the encoded-response LRU (default 64 MiB). It is the encoded
	// share of --mem-budget (see openServer in cmd/timeshadedb/cmd_serve.go).
	CacheBytes int64
	// CacheShards is the number of LRU shards (default 16).
	CacheShards int
	// Compressor configures zstd and gzip.
	Compressor CompressorOptions
	// CoarseZstdLevel is the zstd level for coarse (z < 0) tiles, whose RGB565 bodies
	// are twice the size of paletted z0 bodies. Default zstd.SpeedFastest: on the
	// worst-compressing nauvis z-1 tile it encodes in 1.85 ms against 2.7 ms at
	// SpeedDefault, for 11% more bytes (BenchmarkRealEncode).
	CoarseZstdLevel zstd.EncoderLevel
}

// DefaultCacheBytes is the default encoded-response cache budget.
const DefaultCacheBytes = 64 << 20

// Body is an encoded response. Data may be shared with the cache: never modify it.
type Body struct {
	Data     []byte
	Encoding Encoding
}

type resKind uint8

const (
	resTile resKind = iota + 1
	resDelta
	resPalette
	resPNG
	resCustom
)

// cacheKey identifies one encoded resource. It is comparable and built on the stack, so
// a cache lookup allocates nothing.
type cacheKey struct {
	kind resKind
	enc  Encoding
	z    int8
	x, y int32
	a, b uint32
	ds   core.DatastoreKey
	name string
}

// Renderer serves encoded tiles, deltas, palettes and PNGs from a TileSource (normally
// the pyramid, which covers every level) and a terrain store, through a byte-bounded
// LRU of compressed bodies. Only immutable resources are cached: a (datastore, tile,
// version) never changes content (core.Version), and neither does a palette prefix.
// It is safe for concurrent use.
type Renderer struct {
	src     core.TileSource
	terrain Terrain
	comp    *Compressor
	coarse  *Compressor // z < 0 tiles
	cache   *LRU[cacheKey, []byte]

	palMu sync.Mutex
	pals  map[core.DatastoreKey]*paletteLUT

	flightMu sync.Mutex
	flights  map[cacheKey]*flight
}

type paletteLUT struct {
	pal core.Palette
	lut wire.LUT
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

// NewRenderer returns a renderer over src (tiles at every level) and terrain (palettes,
// live tiles, deltas).
func NewRenderer(src core.TileSource, terrain Terrain, opt Options) (*Renderer, error) {
	if opt.CacheBytes == 0 {
		opt.CacheBytes = DefaultCacheBytes
	}
	if opt.CacheShards <= 0 {
		opt.CacheShards = 16
	}
	comp, err := NewCompressor(opt.Compressor)
	if err != nil {
		return nil, err
	}
	if opt.CoarseZstdLevel == 0 {
		opt.CoarseZstdLevel = zstd.SpeedFastest
	}
	coarse := comp
	if opt.CoarseZstdLevel != comp.zstdLevel {
		co := opt.Compressor
		co.ZstdLevel = opt.CoarseZstdLevel
		if coarse, err = NewCompressor(co); err != nil {
			return nil, err
		}
	}
	return &Renderer{
		src:     src,
		terrain: terrain,
		comp:    comp,
		coarse:  coarse,
		cache:   NewLRU[cacheKey, []byte](opt.CacheBytes, opt.CacheShards, func(b []byte) int64 { return int64(cap(b)) + 96 }),
		pals:    map[core.DatastoreKey]*paletteLUT{},
		flights: map[cacheKey]*flight{},
	}, nil
}

// Close releases the compressor.
func (r *Renderer) Close() error {
	if r.coarse != r.comp {
		r.coarse.Close()
	}
	return r.comp.Close()
}

// Compress compresses an uncached body (for example a mutable JSON response).
func (r *Renderer) Compress(enc Encoding, raw []byte) (Body, error) {
	b, err := r.comp.Compress(enc, raw)
	return Body{Data: b, Encoding: enc}, err
}

// CacheStats returns the encoded-response cache counters (exported at GET /metrics as
// core.MetricEncodedCacheBytes and core.MetricCacheHits/Misses{cache="encoded"}).
func (r *Renderer) CacheStats() LRUStats { return r.cache.Stats() }

// PurgeCache drops every cached body.
func (r *Renderer) PurgeCache() { r.cache.Purge() }

// cached returns the cached body for k or builds it once, even under concurrent
// requests (single flight). Build errors are returned and never cached. The build runs
// with a context that ignores the caller's cancellation, so one aborted request does
// not fail the others waiting for the same body.
func (r *Renderer) cached(ctx context.Context, k cacheKey, build func(ctx context.Context) ([]byte, error)) (Body, error) {
	if b, ok := r.cache.Get(k); ok {
		return Body{Data: b, Encoding: k.enc}, nil
	}
	r.flightMu.Lock()
	// A flight that finished between the Get above and this lock has put its body in
	// the cache before leaving r.flights, so look again rather than build it twice.
	if b, ok := r.cache.Get(k); ok {
		r.flightMu.Unlock()
		return Body{Data: b, Encoding: k.enc}, nil
	}
	if f := r.flights[k]; f != nil {
		r.flightMu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return Body{}, ctx.Err()
		}
		return Body{Data: f.data, Encoding: k.enc}, f.err
	}
	f := &flight{done: make(chan struct{})}
	r.flights[k] = f
	r.flightMu.Unlock()

	raw, err := build(context.WithoutCancel(ctx))
	if err == nil {
		c := r.comp
		if k.kind == resTile && k.z < 0 {
			c = r.coarse
		}
		f.data, err = c.Compress(k.enc, raw)
	}
	f.err = err
	if err == nil {
		if cap(f.data) > len(f.data)+len(f.data)/8+64 {
			// Compressor outputs are allocated for the worst case; keep only what
			// the cache accounts for.
			f.data = append(make([]byte, 0, len(f.data)), f.data...)
		}
		r.cache.Put(k, f.data)
	}
	r.flightMu.Lock()
	delete(r.flights, k)
	r.flightMu.Unlock()
	close(f.done)
	return Body{Data: f.data, Encoding: k.enc}, err
}

// lut returns the datastore palette and its colour lookup table. With refresh it
// re-reads the palette from the terrain store (palettes are append-only, so a newer
// palette is valid for every older tile).
func (r *Renderer) lut(ctx context.Context, ds core.DatastoreKey, refresh bool) (*paletteLUT, error) {
	r.palMu.Lock()
	pl := r.pals[ds]
	r.palMu.Unlock()
	if pl != nil && !refresh {
		return pl, nil
	}
	pal, err := r.terrain.Palette(ctx, ds)
	if err != nil {
		return nil, err
	}
	r.palMu.Lock()
	defer r.palMu.Unlock()
	if cur := r.pals[ds]; cur != nil && len(cur.pal.Colors) >= len(pal.Colors) {
		return cur, nil
	}
	next := &paletteLUT{pal: pal, lut: wire.NewLUT(pal)}
	r.pals[ds] = next
	return next, nil
}

// encodeZ0 encodes a paletted z0 tile, refreshing the palette once if the tile uses a
// colour newer than the cached palette.
func (r *Renderer) encodeZ0(ctx context.Context, ds core.DatastoreKey, h wire.TileHeader, im *core.TileImage) ([]byte, error) {
	pl, err := r.lut(ctx, ds, false)
	if err != nil {
		return nil, err
	}
	out, err := EncodeTile(h, im, pl.lut)
	if errors.Is(err, ErrColour) {
		if pl, err = r.lut(ctx, ds, true); err != nil {
			return nil, err
		}
		out, err = EncodeTile(h, im, pl.lut)
	}
	return out, err
}

func checkTile(tile core.TileCoord) error {
	if !tile.Valid() {
		return fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	return nil
}

// Tile returns the TSV3 body of (ds, tile, v) at any level: paletted at z0, RGB565 at
// z < 0 (FORMAT.md 7.1). Version 0 is all uncharted; versions above the latest return
// core.ErrNoVersion (from the source).
func (r *Renderer) Tile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, v core.Version, enc Encoding) (Body, error) {
	if err := checkTile(tile); err != nil {
		return Body{}, err
	}
	k := cacheKey{kind: resTile, enc: enc, z: tile.Z, x: tile.X, y: tile.Y, a: uint32(v), ds: ds}
	return r.cached(ctx, k, func(ctx context.Context) ([]byte, error) {
		im, err := r.src.ReadVersion(ctx, ds, tile, v)
		if err != nil {
			return nil, err
		}
		h := wire.TileHeader{Z: tile.Z, X: tile.X, Y: tile.Y, Version: v}
		if tile.Z == 0 {
			return r.encodeZ0(ctx, ds, h, im)
		}
		return EncodeTile(h, im, nil)
	})
}

// LiveSource is a TileSource that also serves live content at every level (the
// pyramid: coarse tiles whose z0 descendants changed since their latest version).
type LiveSource interface {
	ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error)
}

// LiveTile returns the TSV3 body of a tile's live content at t (API.md
// "tile/{ds}/{z}/{x}/{y}/live.bin") with the live flag set and the committed version
// underneath. At z0 it is the committed content plus the newest open-window update per
// chunk when live content applies at t (terrain ReadTile). At z < 0 it needs a source
// that implements LiveSource: the average of the committed z0 content not yet in a
// coarse version, or the version effective at t when nothing is pending. It is never
// cached.
func (r *Renderer) LiveTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, enc Encoding) (Body, core.TileState, error) {
	if err := checkTile(tile); err != nil {
		return Body{}, core.TileState{}, err
	}
	if tile.Z < 0 {
		ls, ok := r.src.(LiveSource)
		if !ok {
			return Body{}, core.TileState{}, fmt.Errorf("%w: no live source for coarse tiles: %v", core.ErrBadTile, tile)
		}
		im, st, err := ls.ReadTile(ctx, ds, tile, t, true)
		if err != nil {
			return Body{}, st, err
		}
		raw, err := EncodeTile(wire.TileHeader{Z: tile.Z, X: tile.X, Y: tile.Y, Version: st.Version, Live: true}, im, nil)
		if err != nil {
			return Body{}, st, err
		}
		b, err := r.coarse.Compress(enc, raw)
		return Body{Data: b, Encoding: enc}, st, err
	}
	im, st, err := r.terrain.ReadTile(ctx, ds, tile, t, true)
	if err != nil {
		return Body{}, st, err
	}
	raw, err := r.encodeZ0(ctx, ds, wire.TileHeader{Z: 0, X: tile.X, Y: tile.Y, Version: st.Version, Live: true}, im)
	if err != nil {
		return Body{}, st, err
	}
	b, err := r.Compress(enc, raw)
	return b, st, err
}

// Delta returns the TSD3 body between versions from < to of a z0 tile (FORMAT.md 7.2).
func (r *Renderer) Delta(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, from, to core.Version, enc Encoding) (Body, error) {
	if err := checkTile(tile); err != nil || tile.Z != 0 {
		return Body{}, fmt.Errorf("%w: deltas are z0 only: %v", core.ErrBadTile, tile)
	}
	if from >= to {
		return Body{}, fmt.Errorf("%w: delta %d-%d", core.ErrNoVersion, from, to)
	}
	k := cacheKey{kind: resDelta, enc: enc, x: tile.X, y: tile.Y, a: uint32(from), b: uint32(to), ds: ds}
	return r.cached(ctx, k, func(ctx context.Context) ([]byte, error) {
		deltas, err := r.terrain.ChangedChunks(ctx, ds, tile, from, to)
		if err != nil {
			return nil, err
		}
		h := wire.DeltaHeader{X: tile.X, Y: tile.Y, From: from, To: to}
		pl, err := r.lut(ctx, ds, false)
		if err != nil {
			return nil, err
		}
		out, err := EncodeTileDelta(h, deltas, pl.lut)
		if errors.Is(err, ErrColour) {
			if pl, err = r.lut(ctx, ds, true); err != nil {
				return nil, err
			}
			out, err = EncodeTileDelta(h, deltas, pl.lut)
		}
		return out, err
	})
}

// Palette returns the TSP3 body of the first ver entries of the datastore palette.
// ver above the current palette length returns core.ErrNoVersion.
func (r *Renderer) Palette(ctx context.Context, ds core.DatastoreKey, ver uint32, enc Encoding) (Body, error) {
	if ver < 1 || ver > 65536 {
		return Body{}, fmt.Errorf("%w: palette version %d", core.ErrNoVersion, ver)
	}
	k := cacheKey{kind: resPalette, enc: enc, a: ver, ds: ds}
	return r.cached(ctx, k, func(ctx context.Context) ([]byte, error) {
		pal, err := r.terrain.Palette(ctx, ds)
		if err != nil {
			return nil, err
		}
		return EncodePalette(pal, int(ver))
	})
}

// PNG returns a PNG of (ds, tile, v) for external use (identity encoding: PNG is
// already compressed).
func (r *Renderer) PNG(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, v core.Version) (Body, error) {
	if err := checkTile(tile); err != nil {
		return Body{}, err
	}
	k := cacheKey{kind: resPNG, enc: Identity, z: tile.Z, x: tile.X, y: tile.Y, a: uint32(v), ds: ds}
	return r.cached(ctx, k, func(ctx context.Context) ([]byte, error) {
		im, err := r.src.ReadVersion(ctx, ds, tile, v)
		if err != nil {
			return nil, err
		}
		if tile.Z != 0 {
			return EncodePNG(im, nil, nil)
		}
		pl, err := r.lut(ctx, ds, false)
		if err != nil {
			return nil, err
		}
		out, err := EncodePNG(im, &pl.pal, pl.lut)
		if errors.Is(err, ErrColour) {
			if pl, err = r.lut(ctx, ds, true); err != nil {
				return nil, err
			}
			out, err = EncodePNG(im, &pl.pal, pl.lut)
		}
		return out, err
	})
}

// Cached returns a caller-built body through the same cache. name must identify
// immutable content (for example "timeline/<ds>/<z>/<date>" once final): the first
// build for (name, enc) is kept until evicted.
func (r *Renderer) Cached(ctx context.Context, name string, enc Encoding, build func(ctx context.Context) ([]byte, error)) (Body, error) {
	return r.cached(ctx, cacheKey{kind: resCustom, enc: enc, name: name}, build)
}
