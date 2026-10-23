// Package pyramid keeps the coarse zoom levels (z < 0) of the v3 terrain
// (docs/FORMAT.md 4 and 5.4) and serves every level through core.TileSource.
//
// A pixel at level z-1 is the 2x2 average (core.Average4) of level z, so every coarse
// tile is a pure function of the z0 content at some time. The pyramid stores, per
// coarse tile, one core.Series ("<datastore id>/pyramid/<z>/<x>_<y>") whose records are
// the tile's versions: version n is record n-1, a full snapshot (kind 3) or a delta
// against the previous version (kind 1). Each version is effective at the time T it was
// built for, and its content is exactly the average of the z0 content at T.
//
// Versions are built on a schedule per level (tunable, Options.Intervals; default
// z-1 every 15 min, z-2 every 60 min, z <= -3 every 8 h) when a z0 descendant changed:
//
//   - Update (live) builds at T = the datastore horizon. Versions effective before the
//     horizon never change (core.TileSource), so the z0 content at T is final and a
//     coarse version at T never needs rewriting. That also keeps the immutability rule:
//     new coarse versions become effective at or after the horizon.
//   - Levels are due when T crosses a multiple of their interval; a coarser level is
//     only due together with every finer level, so when a tile is built its children
//     are all current at T. A tile without any version is always due, so new surfaces
//     and freshly charted areas appear at every level at the next Update.
//   - Between scheduled versions, a tile whose z0 descendants changed has a "latest"
//     that is not persisted: the average of the committed z0 content, served through
//     TileState.Live/LiveSeq and ReadTile(live) like z0 live content.
//   - Flush builds every pending tile at the horizon (the latest state reachable).
//   - Rebuild replays an existing z0 history offline (after an import, before serving).
//
// New versions are synced (core.Series.Sync) before readers can see them, so a version
// served under an immutable URL survives a crash. The pyramid holds no series handles
// between calls: reads and builds open the series they need and close them afterwards,
// so the series store bounds open files and index memory (its idle LRU). RAM holds
// about 200 bytes per coarse tile (map entry included, independent of history length)
// plus a byte-bounded LRU of decoded images; version times stay in the series index.
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
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/tileenc"
)

// Terrain is what the pyramid needs from the z0 store. core.TerrainStore satisfies it.
type Terrain interface {
	core.TileSource
	Datastores(ctx context.Context) ([]core.DatastoreInfo, error)
	ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error)
}

// Options configures a Pyramid. Zero values pick the defaults.
type Options struct {
	// MinZ is the coarsest level kept (default core.MinZ).
	MinZ int8
	// Intervals is the minimum spacing of versions per level: Intervals[0] for z-1,
	// [1] for z-2 and so on; the last entry repeats. Each must be a multiple of the
	// previous one. Default DefaultIntervals. Use core.TimeMs(1) to build at every pass.
	Intervals []core.TimeMs
	// CacheBytes bounds the decoded coarse image LRU (default 32 MiB; 512 KiB per image).
	CacheBytes int64
	// MaxChain caps the deltas between snapshots (default 512), bounding a cold read.
	MaxChain int
	// SnapshotRatio: a snapshot replaces a delta when the delta bytes since the last
	// snapshot would exceed this multiple of the snapshot's size (default 64).
	// Measured on the whole illisis test map at 15/60/240 min (TestIllisisTuning):
	// ratio 16 / chain 256 stored 1.98 B per chunk-hour, ratio 32 / chain 512 1.96,
	// ratio 64 / chain 1024 1.91, with cold historical reads at p95 1.0-1.8 ms.
	SnapshotRatio float64
	// ZstdLevel compresses record payloads (default zstd.SpeedBestCompression, about
	// 6% smaller than SpeedDefault on coarse records; snapshots are rare).
	ZstdLevel zstd.EncoderLevel
	// Lazy defers loading each datastore's coarse series (one series open and index read
	// per coarse tile) from New to the first read or build that needs it, so a server can
	// listen at once; call Preload to load them in the background.
	Lazy bool
}

// imageCost is the cache accounting size of one decoded coarse image.
const imageCost = 2*core.TilePixelCount + 64

// DefaultIntervals is the default per-level schedule (FORMAT.md 4): z-1 every 15 min,
// z-2 every 60 min, z <= -3 every 8 h. A 5/15/60 min schedule stored more bytes
// than the disk target (bench/targets.json) allows next to z0 (TestIllisisTuning); between
// versions the live latest (ReadTile) keeps every level current.
var DefaultIntervals = []core.TimeMs{15 * core.Minute, 60 * core.Minute, 480 * core.Minute}

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
	// lazy holds the datastores New found but did not load yet (Options.Lazy).
	lazy map[core.DatastoreKey]struct{}

	buildMu sync.Mutex
	// loading holds a channel per datastore being loaded by ensureDS, closed when done.
	loading map[core.DatastoreKey]chan struct{}
	// lazyErr remembers a deferred datastore whose load failed, so reads return the
	// error until the retry time instead of scanning its series on every request.
	lazyErr map[core.DatastoreKey]lazyFailure
}

var _ core.TileSource = (*Pyramid)(nil)

// imgKey identifies a decoded image: version v of a tile, or (v == 0, live != 0) its
// live latest at live sequence live.
type imgKey struct {
	d    *dsState
	tile core.TileCoord
	v    core.Version
	live uint64
}

type dsState struct {
	key core.DatastoreKey
	id  string

	mu    sync.RWMutex
	tiles map[core.TileCoord]*ctile // every coarse tile known; readers skip n == 0
	// builtT is the last successful build pass (BuiltUpTo), seenT the horizon of the
	// last Update that propagated z0 changes (live content applies at t >= seenT).
	builtT, seenT core.TimeMs

	// Builder-only state (under Pyramid.buildMu).
	z0seen  map[core.TileCoord]core.TimeMs // newest z0 version time propagated, per z0 tile
	dirty   map[core.TileCoord]struct{}
	liveSeq uint64 // last live sequence handed out
}

// ctile is one coarse tile. It holds no series handle.
type ctile struct {
	coord core.TileCoord

	mu sync.RWMutex
	// Published state: versions 1..n are durable and visible to readers.
	n             uint64
	firstT, lastT core.TimeMs
	// descT is the newest z0 version time under the tile, cleanT the last build time
	// that found the content unchanged; the tile is pending (has a live latest that no
	// version holds) when descT > max(lastT, cleanT). liveSeq changes with descT.
	descT, cleanT core.TimeMs
	liveSeq       uint64

	// Builder-only state: the series as appended, which may be ahead of the published
	// state until the next successful Sync.
	bn             uint64
	bfirstT, blast core.TimeMs
	sinceBase      int
	deltaBytes     int64
	baseBytes      int64
	// suspect is set after a failed Append: the builder state is reloaded from the
	// series before the next build.
	suspect bool
}

// view is a consistent copy of a tile's published state.
type view struct {
	n             uint64
	firstT, lastT core.TimeMs
	pending       bool
	liveSeq       uint64
}

func (ct *ctile) pendingLocked() bool {
	return ct.descT != math.MinInt64 && (ct.n == 0 || ct.descT > max(ct.lastT, ct.cleanT))
}

func (ct *ctile) view() view {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return view{n: ct.n, firstT: ct.firstT, lastT: ct.lastT, pending: ct.pendingLocked(), liveSeq: ct.liveSeq}
}

// New opens a pyramid over a terrain store, keeping coarse records in series. It loads
// the coarse tiles of every datastore the terrain store lists (O(coarse tiles) series
// opens and index reads; no pixels are decoded). The caller owns terrain and series and
// closes them after the pyramid.
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
		opt.MaxChain = 512
	}
	if opt.SnapshotRatio <= 0 {
		opt.SnapshotRatio = 64
	}
	if opt.ZstdLevel == 0 {
		opt.ZstdLevel = zstd.SpeedBestCompression
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
		// Every shard must hold a few images, or small budgets would cache nothing.
		cache: tileenc.NewLRU[imgKey, *core.TileImage](opt.CacheBytes, int(min(8, max(1, opt.CacheBytes/(4*imageCost)))), func(*core.TileImage) int64 {
			return imageCost
		}),
		ds: map[core.DatastoreKey]*dsState{},
	}
	infos, err := terrain.Datastores(ctx)
	if err != nil {
		return nil, err
	}
	if opt.Lazy {
		p.lazy = make(map[core.DatastoreKey]struct{}, len(infos))
		for _, info := range infos {
			p.lazy[info.Key] = struct{}{}
		}
		return p, nil
	}
	for _, info := range infos {
		if _, err := p.ensureDS(ctx, info.Key); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// lazyFailure is a failed load of a deferred datastore.
type lazyFailure struct {
	err   error
	until time.Time
}

// lazyRetry is how long a failed deferred load is remembered.
const lazyRetry = 10 * time.Second

// loadDeferred loads a deferred datastore, remembering a failure for lazyRetry.
func (p *Pyramid) loadDeferred(ctx context.Context, key core.DatastoreKey) (*dsState, error) {
	p.mu.RLock()
	f, failed := p.lazyErr[key]
	p.mu.RUnlock()
	if failed && time.Now().Before(f.until) {
		return nil, f.err
	}
	d, err := p.ensureDS(ctx, key)
	if err != nil && ctx.Err() == nil && !errors.Is(err, core.ErrClosed) {
		p.mu.Lock()
		if p.lazyErr == nil {
			p.lazyErr = map[core.DatastoreKey]lazyFailure{}
		}
		p.lazyErr[key] = lazyFailure{err: err, until: time.Now().Add(lazyRetry)}
		p.mu.Unlock()
	}
	return d, err
}

// Preload loads every datastore New deferred (Options.Lazy). It is safe to run while
// the pyramid serves; reads of a datastore not loaded yet load it themselves.
func (p *Pyramid) Preload(ctx context.Context) error {
	p.mu.RLock()
	keys := make([]core.DatastoreKey, 0, len(p.lazy))
	for k := range p.lazy {
		keys = append(keys, k)
	}
	p.mu.RUnlock()
	// Datastores load in parallel (each ensureDS also opens its series in parallel).
	errs := make([]error, len(keys))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			_, errs[i] = p.loadDeferred(ctx, k)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// getDS returns a datastore's state for reading: loaded, or loaded now when New
// deferred it. Datastores New did not see and no build loaded are unknown (nil), so
// hostile ids never allocate.
func (p *Pyramid) getDS(key core.DatastoreKey) (*dsState, bool, error) {
	p.mu.RLock()
	closed, d := p.closed, p.ds[key]
	_, deferred := p.lazy[key]
	p.mu.RUnlock()
	if closed {
		return nil, true, core.ErrClosed
	}
	if d != nil || !deferred {
		return d, false, nil
	}
	d, err := p.loadDeferred(context.Background(), key)
	return d, false, err
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

// loadState reads a series' version bookkeeping into the builder state of ct.
func loadState(sr core.Series, ct *ctile) error {
	ct.bn, ct.bfirstT, ct.blast = sr.Len(), 0, math.MinInt64
	ct.sinceBase, ct.deltaBytes, ct.baseBytes = 0, 0, 0
	if ct.bn == 0 {
		return nil
	}
	first, err := sr.Meta(0)
	if err != nil {
		return err
	}
	ct.bfirstT = first.TEnd()
	for s := ct.bn; s > 0; s-- {
		m, err := sr.Meta(s - 1)
		if err != nil {
			return err
		}
		if s == ct.bn {
			ct.blast = m.TEnd()
		}
		if m.Kind.IsBase() {
			ct.baseBytes = int64(m.Len)
			break
		}
		ct.sinceBase++
		ct.deltaBytes += int64(m.Len)
	}
	return nil
}

// loadOpenWorkers bounds the series ensureDS opens at once.
const loadOpenWorkers = 16

// loadTiles runs loadState for every tile, opening and closing its series, with up to
// loadOpenWorkers at a time. It returns the first error.
func (p *Pyramid) loadTiles(ctx context.Context, tiles []*ctile, names []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		first   error
	)
	fail := func(err error) {
		errOnce.Do(func() { first = err; cancel() })
	}
	jobs := make(chan int)
	for w := 0; w < min(loadOpenWorkers, len(tiles)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sr, err := p.series.Open(ctx, names[i], false)
				if err != nil {
					fail(fmt.Errorf("pyramid: open %s: %w", names[i], err))
					continue
				}
				err = loadState(sr, tiles[i])
				if e := sr.Close(); err == nil {
					err = e
				}
				if err != nil {
					fail(fmt.Errorf("pyramid: load %s: %w", names[i], err))
				}
			}
		}()
	}
feed:
	for i := range tiles {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// newTile returns an empty coarse tile.
func newTile(c core.TileCoord) *ctile {
	return &ctile{coord: c, lastT: math.MinInt64, blast: math.MinInt64, descT: math.MinInt64, cleanT: math.MinInt64}
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
	// Concurrent first reads of a datastore load it once; loads of different datastores
	// run in parallel, so a read waits only for its own datastore.
	p.mu.Lock()
	if d = p.ds[key]; d != nil {
		p.mu.Unlock()
		return d, nil
	}
	if ch, ok := p.loading[key]; ok {
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return p.ensureDS(ctx, key)
	}
	if p.loading == nil {
		p.loading = map[core.DatastoreKey]chan struct{}{}
	}
	ch := make(chan struct{})
	p.loading[key] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.loading, key)
		p.mu.Unlock()
		close(ch)
	}()
	d = &dsState{
		key:    key,
		id:     key.ID(),
		tiles:  map[core.TileCoord]*ctile{},
		z0seen: map[core.TileCoord]core.TimeMs{},
		dirty:  map[core.TileCoord]struct{}{},
		builtT: math.MinInt64,
		seenT:  math.MinInt64,
	}
	prefix := d.id + "/pyramid/"
	names, err := p.series.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var tiles []*ctile
	var tileNames []string
	for _, name := range names {
		tile, ok := parseSeriesName(prefix, name)
		if !ok || tile.Z < p.opt.MinZ {
			continue
		}
		tiles = append(tiles, newTile(tile))
		tileNames = append(tileNames, name)
	}
	// Opening a series is mostly waiting on the file system (index read, last record
	// check), and much slower on Windows while files are fresh from a bulk write, so
	// the series of a datastore are loaded in parallel.
	if err := p.loadTiles(ctx, tiles, tileNames); err != nil {
		return nil, err
	}
	for _, ct := range tiles {
		// Records found at open were synced by the store or survived its recovery.
		ct.n, ct.firstT, ct.lastT = ct.bn, ct.bfirstT, ct.blast
		if ct.n > 0 {
			ct.descT = ct.lastT
			d.builtT = max(d.builtT, ct.lastT)
		}
		d.tiles[ct.coord] = ct
	}
	d.seenT = d.builtT
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, core.ErrClosed
	}
	delete(p.lazy, key)
	delete(p.lazyErr, key)
	if cur := p.ds[key]; cur != nil {
		return cur, nil
	}
	p.ds[key] = d
	return d, nil
}

// lookup resolves a coarse tile for reading. It never allocates per-tile state.
func (p *Pyramid) lookup(key core.DatastoreKey, tile core.TileCoord) (*dsState, *ctile, view, error) {
	if !tile.Valid() || tile.Z < p.opt.MinZ {
		return nil, nil, view{}, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	d, closed, err := p.getDS(key)
	if closed {
		return nil, nil, view{}, core.ErrClosed
	}
	if err != nil {
		return nil, nil, view{}, err
	}
	if d == nil {
		return nil, nil, view{}, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	d.mu.RLock()
	ct := d.tiles[tile]
	d.mu.RUnlock()
	if ct == nil {
		return d, nil, view{}, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	v := ct.view()
	if v.n == 0 {
		return d, nil, view{}, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	return d, ct, v, nil
}

// open opens a coarse tile's series for reading.
func (p *Pyramid) open(ctx context.Context, d *dsState, tile core.TileCoord) (core.Series, error) {
	return p.series.Open(ctx, seriesName(d.id, tile), false)
}

// versionAt returns the number of records among the first n with TEnd <= t.
func versionAt(sr core.Series, n uint64, t core.TimeMs) (core.Version, error) {
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

// versionAtView resolves VersionAt from a published view, opening the series only for
// times between the first and the latest version.
func (p *Pyramid) versionAtView(ctx context.Context, d *dsState, tile core.TileCoord, v view, t core.TimeMs) (core.Version, error) {
	if t >= v.lastT {
		return core.Version(v.n), nil
	}
	if t < v.firstT {
		return 0, nil
	}
	sr, err := p.open(ctx, d, tile)
	if err != nil {
		return 0, err
	}
	defer sr.Close()
	return versionAt(sr, v.n, t)
}

// liveAt reports whether the live latest applies to a tile at t.
func (d *dsState) liveAt(v view, t core.TimeMs) bool {
	if !v.pending {
		return false
	}
	d.mu.RLock()
	seen := d.seenT
	d.mu.RUnlock()
	return seen != math.MinInt64 && t >= seen
}

// VersionAt implements core.TileSource. At t at or after the horizon of the last
// Update, a coarse tile whose z0 descendants changed since its latest version reports
// Live and a LiveSeq; ReadTile with live returns that content.
func (p *Pyramid) VersionAt(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs) (core.TileState, error) {
	if tile.Z == 0 {
		return p.terrain.VersionAt(ctx, key, tile, t)
	}
	d, _, v, err := p.lookup(key, tile)
	if err != nil {
		return core.TileState{}, err
	}
	ver, err := p.versionAtView(ctx, d, tile, v, t)
	if err != nil {
		return core.TileState{}, err
	}
	st := core.TileState{Version: ver}
	if d.liveAt(v, t) {
		st.Live, st.LiveSeq = true, v.liveSeq
	}
	return st, nil
}

// ReadVersion implements core.TileSource. The returned image is the caller's.
func (p *Pyramid) ReadVersion(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, v core.Version) (*core.TileImage, error) {
	if tile.Z == 0 {
		return p.terrain.ReadVersion(ctx, key, tile, v)
	}
	d, _, vw, err := p.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if uint64(v) > vw.n {
		return nil, fmt.Errorf("%w: %v %v v%d", core.ErrNoVersion, d.key, tile, v)
	}
	im, err := p.read(ctx, d, tile, nil, v)
	if err != nil {
		return nil, err
	}
	return im.Clone(), nil
}

// ReadTile returns a tile's content at t, like core.TerrainStore.ReadTile, at every
// level: z0 is delegated to the terrain store. At z < 0, without live (or when the
// live latest does not apply at t) it is the content of VersionAt(t).Version; with live
// and TileState.Live it is the average of the committed z0 content as of the last
// Update, which no version holds yet. That content is computed on demand (cached by
// LiveSeq) and never persisted. The returned image is the caller's.
func (p *Pyramid) ReadTile(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error) {
	if tile.Z == 0 {
		return p.terrain.ReadTile(ctx, key, tile, t, live)
	}
	d, ct, v, err := p.lookup(key, tile)
	if err != nil {
		return nil, core.TileState{}, err
	}
	ver, err := p.versionAtView(ctx, d, tile, v, t)
	if err != nil {
		return nil, core.TileState{}, err
	}
	st := core.TileState{Version: ver}
	var im *core.TileImage
	if d.liveAt(v, t) {
		st.Live, st.LiveSeq = true, v.liveSeq
		if live {
			im, err = p.latest(ctx, d, ct)
		}
	}
	if im == nil && err == nil {
		im, err = p.read(ctx, d, tile, nil, ver)
	}
	if err != nil {
		return nil, st, err
	}
	return im.Clone(), st, nil
}

// latest returns the newest content of a coarse tile: its latest version when nothing
// is pending, else the average of its children's newest content (z0: committed content
// at the tile's descT, which covers every z0 change propagated under it). Shared with
// the cache (read-only).
func (p *Pyramid) latest(ctx context.Context, d *dsState, ct *ctile) (*core.TileImage, error) {
	ct.mu.RLock()
	n, pending, seq, descT := ct.n, ct.pendingLocked(), ct.liveSeq, ct.descT
	ct.mu.RUnlock()
	if !pending {
		return p.read(ctx, d, ct.coord, nil, core.Version(n))
	}
	k := imgKey{d: d, tile: ct.coord, live: seq}
	if im, ok := p.cache.Get(k); ok {
		return im, nil
	}
	var kids [4]*core.TileImage
	for i, c := range ct.coord.Children() {
		var err error
		if c.Z == 0 {
			kids[i], err = p.z0At(ctx, d.key, c, descT)
		} else {
			d.mu.RLock()
			child := d.tiles[c]
			d.mu.RUnlock()
			if child != nil {
				kids[i], err = p.latest(ctx, d, child)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	im := downsample(kids)
	p.cache.Put(k, im)
	return im, nil
}

// read returns version v of a coarse tile, shared with the cache (read-only). sr is an
// open handle to the tile's series, or nil to open one when the cache misses. The
// caller checks that v exists.
func (p *Pyramid) read(ctx context.Context, d *dsState, tile core.TileCoord, sr core.Series, v core.Version) (*core.TileImage, error) {
	if v == 0 {
		return core.NewTileImage(), nil
	}
	k := imgKey{d: d, tile: tile, v: v}
	if im, ok := p.cache.Get(k); ok {
		return im, nil
	}
	if sr == nil {
		var err error
		if sr, err = p.open(ctx, d, tile); err != nil {
			return nil, err
		}
		defer sr.Close()
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
		return nil, fmt.Errorf("%w: %v %v: chain for v%d does not end at record %d", errPayload, d.key, tile, v, v-1)
	}
	im := core.NewTileImage()
	buf := make([]byte, 0, 2*core.TilePixelCount)
	for i, r := range recs {
		raw, err := p.dec.DecodeAll(r.Payload, buf[:0])
		if err != nil {
			return nil, fmt.Errorf("%w: %v %v record %d: %v", errPayload, d.key, tile, r.Seq, err)
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
			return nil, fmt.Errorf("%v %v record %d: %w", d.key, tile, r.Seq, err)
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
	d, _, v, err := p.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if t0 >= t1 || t1 <= v.firstT || t0 > v.lastT {
		return nil, nil
	}
	sr, err := p.open(ctx, d, tile)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	if t0 == math.MinInt64 {
		t0 = math.MinInt64 + 1
	}
	start, err := versionAt(sr, v.n, t0-1) // first record with TEnd >= t0
	if err != nil {
		return nil, err
	}
	var out []core.VersionPoint
	for s := uint64(start); s < v.n; s++ {
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
// version, sorted by (y, x). Live is set for tiles with a pending live latest.
func (p *Pyramid) ListTiles(ctx context.Context, key core.DatastoreKey, z int8) ([]core.TileInfo, error) {
	if z == 0 {
		return p.terrain.ListTiles(ctx, key, 0)
	}
	if z > 0 || z < p.opt.MinZ {
		return nil, fmt.Errorf("%w: level %d", core.ErrBadTile, z)
	}
	d, closed, err := p.getDS(key)
	if closed {
		return nil, core.ErrClosed
	}
	if err != nil {
		return nil, err
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
		if v := ct.view(); v.n > 0 {
			out = append(out, core.TileInfo{Tile: ct.coord, Version: core.Version(v.n), FirstT: v.firstT, LastT: v.lastT, Live: v.pending})
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

// BuiltUpTo returns the time up to which the coarse levels of a datastore are final:
// no build will add a coarse version effective before it, so VersionAt(t) for t below
// it never changes. It is the last successful build pass (normally the horizon at the
// last Update) and can lag the terrain horizon, so the API must use min(horizon,
// BuiltUpTo) when it decides that a coarse answer is immutable (API.md "view"). ok is
// false before anything was built.
func (p *Pyramid) BuiltUpTo(key core.DatastoreKey) (t core.TimeMs, ok bool) {
	d, _, err := p.getDS(key)
	if err != nil || d == nil {
		return 0, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.builtT, d.builtT != math.MinInt64
}

// CacheStats returns the decoded coarse image cache counters (exported at GET /metrics under
// core.MetricDecodedCacheBytes and core.MetricCacheHits{cache="decoded"}).
func (p *Pyramid) CacheStats() tileenc.LRUStats { return p.cache.Stats() }

// PurgeCache drops every decoded image (benchmarks use it to measure cold reads).
func (p *Pyramid) PurgeCache() { p.cache.Purge() }

// Close stops the pyramid. It holds no series handles; the terrain and series stores
// stay open.
func (p *Pyramid) Close() error {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.dec.Close()
	err := p.enc.Close()
	p.cache.Purge()
	return err
}
