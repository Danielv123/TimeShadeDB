// Package pyramid keeps the coarse zoom levels (z < 0) of the v3 terrain (PLAN.md §4.5,
// docs/v3/FORMAT.md 4 and 5.4, WP3) and serves every level through core.TileSource.
//
// A pixel at level z-1 is the 2x2 average (core.Average4) of level z, so every coarse
// tile is a pure function of the z0 content at some time. The pyramid stores, per
// coarse tile, one core.Series ("<datastore id>/pyramid/<z>/<x>_<y>") whose records are
// the tile's versions: version n is record n-1, a full snapshot (kind 3) or a delta
// against the previous version (kind 1). Each version is effective at the time T it was
// built for, and its content is exactly the average of the z0 content at T.
//
// Versions are built on a schedule per level (tunable, Options.Intervals; default
// z-1 every 5 min, z-2 every 15 min, z <= -3 every 60 min) when a z0 descendant changed:
//
//   - Update (live) builds at T = the datastore horizon. Versions effective before the
//     horizon never change (core.TileSource), so the z0 content at T is final and a
//     coarse version at T never needs rewriting. That also keeps the immutability rule:
//     new coarse versions become effective at or after the horizon.
//   - Levels are due when T crosses a multiple of their interval; a coarser level is
//     only due together with every finer level, so when a tile is built its children
//     are all current at T.
//   - Flush builds every pending tile at the horizon (the latest state reachable).
//   - Rebuild replays an existing z0 history offline (after an import, before serving).
//
// RAM holds about 100 bytes per coarse tile plus a byte-bounded LRU of decoded images;
// version times stay in the series index.
package pyramid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/tileenc"
)

// Terrain is what the pyramid needs from the z0 store. core.TerrainStore satisfies it.
type Terrain interface {
	core.TileSource
	Datastores(ctx context.Context) ([]core.DatastoreInfo, error)
}

// Options configures a Pyramid. Zero values pick the defaults.
type Options struct {
	// MinZ is the coarsest level kept (default core.MinZ).
	MinZ int8
	// Intervals is the minimum spacing of versions per level: Intervals[0] for z-1,
	// [1] for z-2 and so on; the last entry repeats. Each must be a multiple of the
	// previous one. Default 5 min, 15 min, 60 min. Use core.TimeMs(1) to build at every
	// pass.
	Intervals []core.TimeMs
	// CacheBytes bounds the decoded coarse image LRU (default 32 MiB; 512 KiB per image).
	CacheBytes int64
	// MaxChain caps the deltas between snapshots (default 128), bounding a cold read.
	MaxChain int
	// SnapshotRatio: a snapshot replaces a delta when the delta bytes since the last
	// snapshot would exceed this multiple of the snapshot's size (default 4).
	SnapshotRatio float64
	// ZstdLevel compresses record payloads (default zstd.SpeedDefault).
	ZstdLevel zstd.EncoderLevel
}

// DefaultIntervals is the default per-level schedule (FORMAT.md 4).
var DefaultIntervals = []core.TimeMs{5 * core.Minute, 15 * core.Minute, 60 * core.Minute}

// ErrAlreadyBuilt is returned by Rebuild for a datastore that already has coarse records.
var ErrAlreadyBuilt = errors.New("pyramid: coarse levels already built")

// Pyramid implements core.TileSource for every level z in [MinZ, 0]: z0 reads go to the
// terrain store, coarse reads to the pyramid's own series. It is safe for concurrent
// readers; builds (Update, Flush, Rebuild) are serialised internally.
type Pyramid struct {
	terrain Terrain
	series  core.SeriesStore
	opt     Options
	enc     *zstd.Encoder
	dec     *zstd.Decoder
	cache   *tileenc.LRU[imgKey, *core.TileImage]

	mu     sync.RWMutex
	closed bool
	ds     map[core.DatastoreKey]*dsState

	buildMu sync.Mutex
}

var _ core.TileSource = (*Pyramid)(nil)

type imgKey struct {
	d    *dsState
	tile core.TileCoord
	v    core.Version
}

type dsState struct {
	key core.DatastoreKey
	id  string

	mu    sync.RWMutex
	tiles map[core.TileCoord]*ctile // every coarse tile known; readers skip n == 0

	// Builder-only state (under Pyramid.buildMu).
	z0seen   map[core.TileCoord]core.TimeMs // newest z0 version time propagated, per z0 tile
	dirty    map[core.TileCoord]struct{}
	prevT    core.TimeMs // time of the previous live pass (math.MinInt64: none yet)
}

// ctile is one coarse tile.
type ctile struct {
	coord core.TileCoord

	mu            sync.RWMutex
	sr            core.Series
	n             uint64 // versions (= records)
	firstT, lastT core.TimeMs

	// Builder-only state.
	descT      core.TimeMs // newest z0 version time under the tile
	cleanT     core.TimeMs // last build time that found the content unchanged
	sinceBase  int
	deltaBytes int64
	baseBytes  int64
}

func (ct *ctile) state() (core.Series, uint64, core.TimeMs, core.TimeMs) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return ct.sr, ct.n, ct.firstT, ct.lastT
}

// New opens a pyramid over a terrain store, keeping coarse records in series. It loads
// the coarse tiles of every datastore the terrain store lists (O(coarse tiles); no
// pixels are decoded). The caller owns terrain and series and closes them after the
// pyramid.
func New(ctx context.Context, terrain Terrain, series core.SeriesStore, opt Options) (*Pyramid, error) {
	if opt.MinZ == 0 {
		opt.MinZ = core.MinZ
	}
	if opt.MinZ > -1 || opt.MinZ < core.MinZ {
		return nil, fmt.Errorf("pyramid: MinZ %d outside [%d, -1]", opt.MinZ, core.MinZ)
	}
	if len(opt.Intervals) == 0 {
		opt.Intervals = DefaultIntervals
	}
	for i, iv := range opt.Intervals {
		if iv <= 0 || (i > 0 && iv%opt.Intervals[i-1] != 0) {
			return nil, fmt.Errorf("pyramid: interval %d (%d ms) must be positive and a multiple of the previous one", i, iv)
		}
	}
	if opt.CacheBytes == 0 {
		opt.CacheBytes = 32 << 20
	}
	if opt.MaxChain <= 0 {
		opt.MaxChain = 128
	}
	if opt.SnapshotRatio <= 0 {
		opt.SnapshotRatio = 4
	}
	if opt.ZstdLevel == 0 {
		opt.ZstdLevel = zstd.SpeedDefault
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(opt.ZstdLevel), zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<20), zstd.WithLowerEncoderMem(true))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		return nil, err
	}
	p := &Pyramid{
		terrain: terrain,
		series:  series,
		opt:     opt,
		enc:     enc,
		dec:     dec,
		cache: tileenc.NewLRU[imgKey, *core.TileImage](opt.CacheBytes, 8, func(*core.TileImage) int64 {
			return 2*core.TilePixelCount + 64
		}),
		ds: map[core.DatastoreKey]*dsState{},
	}
	infos, err := terrain.Datastores(ctx)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if _, err := p.ensureDS(ctx, info.Key); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// interval returns the version spacing of level index k = -z (k >= 1).
func (p *Pyramid) interval(k int) core.TimeMs {
	if k-1 < len(p.opt.Intervals) {
		return p.opt.Intervals[k-1]
	}
	return p.opt.Intervals[len(p.opt.Intervals)-1]
}

func (p *Pyramid) levels() int { return -int(p.opt.MinZ) }

func seriesName(id string, tile core.TileCoord) string {
	return id + "/pyramid/" + strconv.Itoa(int(tile.Z)) + "/" + strconv.Itoa(int(tile.X)) + "_" + strconv.Itoa(int(tile.Y))
}

func parseSeriesName(prefix, name string) (core.TileCoord, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return core.TileCoord{}, false
	}
	zs, xy, ok := strings.Cut(rest, "/")
	if !ok {
		return core.TileCoord{}, false
	}
	xs, ys, ok := strings.Cut(xy, "_")
	if !ok {
		return core.TileCoord{}, false
	}
	z, e1 := strconv.Atoi(zs)
	x, e2 := strconv.Atoi(xs)
	y, e3 := strconv.Atoi(ys)
	if e1 != nil || e2 != nil || e3 != nil || z >= 0 || z < core.MinZ {
		return core.TileCoord{}, false
	}
	t := core.TileCoord{Z: int8(z), X: int32(x), Y: int32(y)}
	return t, t.Valid() && seriesName("", t) == "/pyramid/"+rest
}

// ensureDS returns the datastore state, loading its existing coarse series the first
// time. Only datastores the terrain store reported get here, so hostile ids never
// allocate.
func (p *Pyramid) ensureDS(ctx context.Context, key core.DatastoreKey) (*dsState, error) {
	p.mu.RLock()
	d := p.ds[key]
	p.mu.RUnlock()
	if d != nil {
		return d, nil
	}
	d = &dsState{
		key:      key,
		id:       key.ID(),
		tiles:    map[core.TileCoord]*ctile{},
		z0seen:   map[core.TileCoord]core.TimeMs{},
		dirty:    map[core.TileCoord]struct{}{},
		prevT:    math.MinInt64,
	}
	prefix := d.id + "/pyramid/"
	names, err := p.series.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		tile, ok := parseSeriesName(prefix, name)
		if !ok || tile.Z < p.opt.MinZ {
			continue
		}
		sr, err := p.series.Open(ctx, name, false)
		if err != nil {
			return nil, fmt.Errorf("pyramid: open %s: %w", name, err)
		}
		ct := &ctile{coord: tile, sr: sr, n: sr.Len(), descT: math.MinInt64, cleanT: math.MinInt64}
		if ct.n > 0 {
			first, err := sr.Meta(0)
			if err != nil {
				return nil, err
			}
			ct.firstT = first.TEnd()
			for s := ct.n; s > 0; s-- {
				m, err := sr.Meta(s - 1)
				if err != nil {
					return nil, err
				}
				if s == ct.n {
					ct.lastT = m.TEnd()
				}
				if m.Kind.IsBase() {
					ct.baseBytes = int64(m.Len)
					break
				}
				ct.sinceBase++
				ct.deltaBytes += int64(m.Len)
			}
			ct.descT = ct.lastT
		}
		d.tiles[tile] = ct
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, core.ErrClosed
	}
	if cur := p.ds[key]; cur != nil {
		return cur, nil
	}
	p.ds[key] = d
	return d, nil
}

// lookup resolves a coarse tile for reading. It never allocates per-tile state.
func (p *Pyramid) lookup(key core.DatastoreKey, tile core.TileCoord) (*dsState, *ctile, error) {
	if !tile.Valid() || tile.Z < p.opt.MinZ {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	p.mu.RLock()
	closed, d := p.closed, p.ds[key]
	p.mu.RUnlock()
	if closed {
		return nil, nil, core.ErrClosed
	}
	if d == nil {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	d.mu.RLock()
	ct := d.tiles[tile]
	d.mu.RUnlock()
	if ct == nil {
		return d, nil, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	if _, n, _, _ := ct.state(); n == 0 {
		return d, nil, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	return d, ct, nil
}

// versionAt returns the number of records with TEnd <= t.
func versionAt(sr core.Series, n uint64, lastT core.TimeMs, t core.TimeMs) (core.Version, error) {
	if t >= lastT {
		return core.Version(n), nil
	}
	lo, hi := uint64(0), n // first seq with TEnd > t is in [lo, hi]
	for lo < hi {
		mid := lo + (hi-lo)/2
		m, err := sr.Meta(mid)
		if err != nil {
			return 0, err
		}
		if m.TEnd() > t {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return core.Version(lo), nil
}

// VersionAt implements core.TileSource. Coarse tiles have no live content.
func (p *Pyramid) VersionAt(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs) (core.TileState, error) {
	if tile.Z == 0 {
		return p.terrain.VersionAt(ctx, key, tile, t)
	}
	_, ct, err := p.lookup(key, tile)
	if err != nil {
		return core.TileState{}, err
	}
	sr, n, _, lastT := ct.state()
	v, err := versionAt(sr, n, lastT, t)
	return core.TileState{Version: v}, err
}

// ReadVersion implements core.TileSource. The returned image is the caller's.
func (p *Pyramid) ReadVersion(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, v core.Version) (*core.TileImage, error) {
	if tile.Z == 0 {
		return p.terrain.ReadVersion(ctx, key, tile, v)
	}
	d, ct, err := p.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	im, err := p.read(ctx, d, ct, v)
	if err != nil {
		return nil, err
	}
	return im.Clone(), nil
}

// read returns version v of a coarse tile, shared with the cache (read-only).
func (p *Pyramid) read(ctx context.Context, d *dsState, ct *ctile, v core.Version) (*core.TileImage, error) {
	sr, n, _, _ := ct.state()
	if uint64(v) > n {
		return nil, fmt.Errorf("%w: %v %v v%d", core.ErrNoVersion, d.key, ct.coord, v)
	}
	if v == 0 {
		return core.NewTileImage(), nil
	}
	k := imgKey{d: d, tile: ct.coord, v: v}
	if im, ok := p.cache.Get(k); ok {
		return im, nil
	}
	m, err := sr.Meta(uint64(v) - 1)
	if err != nil {
		return nil, err
	}
	recs, err := sr.Chain(ctx, m.TEnd())
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 || recs[len(recs)-1].Seq != uint64(v)-1 || !recs[0].Kind.IsBase() {
		return nil, fmt.Errorf("%w: %v %v: chain for v%d does not end at record %d", errPayload, d.key, ct.coord, v, v-1)
	}
	im := core.NewTileImage()
	buf := make([]byte, 0, 2*core.TilePixelCount)
	for i, r := range recs {
		raw, err := p.dec.DecodeAll(r.Payload, buf[:0])
		if err != nil {
			return nil, fmt.Errorf("%w: %v %v record %d: %v", errPayload, d.key, ct.coord, r.Seq, err)
		}
		switch {
		case r.Kind.IsBase():
			if i != 0 {
				return nil, fmt.Errorf("%w: base record inside chain", errPayload)
			}
			err = decodeSnapshot(raw, im)
		case r.Kind == core.RecordDelta:
			err = applyDelta(raw, im)
		default:
			err = fmt.Errorf("%w: record kind %d", errPayload, r.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("%v %v record %d: %w", d.key, ct.coord, r.Seq, err)
		}
		if cap(raw) > cap(buf) {
			buf = raw
		}
	}
	p.cache.Put(k, im)
	return im, nil
}

// History implements core.TileSource.
func (p *Pyramid) History(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t0, t1 core.TimeMs) ([]core.VersionPoint, error) {
	if tile.Z == 0 {
		return p.terrain.History(ctx, key, tile, t0, t1)
	}
	_, ct, err := p.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	sr, n, _, lastT := ct.state()
	if t0 == math.MinInt64 {
		t0 = math.MinInt64 + 1
	}
	start, err := versionAt(sr, n, lastT, t0-1) // first record with TEnd >= t0
	if err != nil {
		return nil, err
	}
	var out []core.VersionPoint
	for s := uint64(start); s < n; s++ {
		m, err := sr.Meta(s)
		if err != nil {
			return nil, err
		}
		if m.TEnd() >= t1 {
			break
		}
		out = append(out, core.VersionPoint{T: m.TEnd(), Version: core.Version(s + 1)})
	}
	return out, nil
}

// ListTiles implements core.TileSource: at z < 0, every coarse tile with at least one
// version, sorted by (y, x).
func (p *Pyramid) ListTiles(ctx context.Context, key core.DatastoreKey, z int8) ([]core.TileInfo, error) {
	if z == 0 {
		return p.terrain.ListTiles(ctx, key, 0)
	}
	if z > 0 || z < p.opt.MinZ {
		return nil, fmt.Errorf("%w: level %d", core.ErrBadTile, z)
	}
	p.mu.RLock()
	closed, d := p.closed, p.ds[key]
	p.mu.RUnlock()
	if closed {
		return nil, core.ErrClosed
	}
	if d == nil {
		return nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	d.mu.RLock()
	cts := make([]*ctile, 0, len(d.tiles))
	for c, ct := range d.tiles {
		if c.Z == z {
			cts = append(cts, ct)
		}
	}
	d.mu.RUnlock()
	out := make([]core.TileInfo, 0, len(cts))
	for _, ct := range cts {
		_, n, first, last := ct.state()
		if n > 0 {
			out = append(out, core.TileInfo{Tile: ct.coord, Version: core.Version(n), FirstT: first, LastT: last})
		}
	}
	slices.SortFunc(out, func(a, b core.TileInfo) int {
		if a.Tile.Y != b.Tile.Y {
			return int(a.Tile.Y) - int(b.Tile.Y)
		}
		return int(a.Tile.X) - int(b.Tile.X)
	})
	return out, nil
}

// CacheStats returns the decoded coarse image cache counters (exported by WP6 under
// core.MetricDecodedCacheBytes and core.MetricCacheHits{cache="decoded"}).
func (p *Pyramid) CacheStats() tileenc.LRUStats { return p.cache.Stats() }

// PurgeCache drops every decoded image (benchmarks use it to measure cold reads).
func (p *Pyramid) PurgeCache() { p.cache.Purge() }

// Close closes the pyramid's series handles. The terrain and series stores stay open.
func (p *Pyramid) Close() error {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var first error
	for _, d := range p.ds {
		for _, ct := range d.tiles {
			if ct.sr != nil {
				if err := ct.sr.Close(); err != nil && first == nil {
					first = err
				}
			}
		}
	}
	p.dec.Close()
	if err := p.enc.Close(); err != nil && first == nil {
		first = err
	}
	p.cache.Purge()
	return first
}
