// Package terrain is the z0 terrain store of TimeShadeDB v3 (PLAN.md §4.2, WP2). It
// implements core.TerrainStore on top of the series package:
//
//   - one series per (datastore, z0 tile) under <db>/datastores/<id>/tiles/<x>_<y>;
//   - a delta record per tile and closed window with the net change of that window, in
//     palette indices (FORMAT.md 5.3), and keyframes chosen by a byte rule;
//   - an append-only palette per datastore;
//   - open windows in memory (the newest update per chunk and window) backed by a
//     write-ahead log, bounded by a byte budget;
//   - a decoded-tile LRU bounded by bytes, and an LRU of open series (indexes) bounded
//     by count, so memory does not grow with history.
//
// Startup reads each tile's index (no history is decoded) and replays the WAL.
package terrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/series"
)

// noHorizon marks a datastore that was never committed.
const noHorizon = core.TimeMs(math.MinInt64)

// openChunkBytes is the accounted size of one open-window chunk entry.
const openChunkBytes = core.ChunkPixelCount*2 + 64

// ErrOverBudget is returned by Apply when the open-window buffers are over their byte
// budget. Nothing from the batch was applied; the caller should retry later (HTTP 503).
var ErrOverBudget = errors.New("terrain: open-window budget exceeded")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func crcC(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// Options configure a Store. Zero values select the defaults.
type Options struct {
	// Window is the window length for new datastores (default core.DefaultTerrainWindow).
	Window core.TimeMs
	// DecodedCacheBytes bounds the decoded-tile LRU (default 64 MiB; < 0 disables it).
	DecodedCacheBytes int64
	// OpenWindowBytes is the budget for open-window buffers (default 64 MiB; < 0 means
	// unlimited). Apply returns ErrOverBudget while it is exceeded.
	OpenWindowBytes int64
	// MaxOpenSeries bounds the idle series (indexes and file handles) kept open
	// (default 256).
	MaxOpenSeries int
	// SyncInterval is the WAL group-commit interval (default 1 s).
	SyncInterval time.Duration
	// NoSync skips every fsync (tests, bulk imports that can be redone).
	NoSync bool
	// NoWAL disables the write-ahead log: open windows are lost on a crash or Close.
	NoWAL bool
	// Level is the zstd level for terrain records (default zstd.SpeedDefault).
	Level zstd.EncoderLevel
	// KeyframeFactor: a keyframe is written when the delta bytes since the last base
	// reach KeyframeFactor times its size (default 2; FORMAT.md 5.3).
	KeyframeFactor float64
	// MaxChain caps the records between keyframes (default 256).
	MaxChain int
	// Workers is the number of tiles a commit encodes in parallel (default GOMAXPROCS).
	Workers int
	// Logf receives recovery notes (default: discard).
	Logf func(format string, args ...any)
}

// SplitBudget returns options with a total memory budget split between the decoded
// cache (half), open windows (a quarter) and series indexes (the rest, as a count).
// Callers that also run an encoded-response cache should pass only this store's share.
func SplitBudget(total int64) Options {
	return Options{
		DecodedCacheBytes: total / 2,
		OpenWindowBytes:   total / 4,
		MaxOpenSeries:     int(max(64, total/4/(64<<10))),
	}
}

func (o *Options) defaults() {
	if o.Window <= 0 {
		o.Window = core.DefaultTerrainWindow
	}
	if o.DecodedCacheBytes == 0 {
		o.DecodedCacheBytes = 64 << 20
	}
	if o.OpenWindowBytes == 0 {
		o.OpenWindowBytes = 64 << 20
	}
	if o.MaxOpenSeries <= 0 {
		o.MaxOpenSeries = 256
	}
	if o.SyncInterval <= 0 {
		o.SyncInterval = time.Second
	}
	if o.Level == 0 {
		o.Level = zstd.SpeedDefault
	}
	if o.KeyframeFactor <= 0 {
		o.KeyframeFactor = 2
	}
	if o.MaxChain <= 0 {
		o.MaxChain = 256
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// Store is the on-disk terrain store.
type Store struct {
	dir    string
	opt    Options
	series *series.Store
	enc    *zstd.Encoder
	dec    *zstd.Decoder
	cache  *decodedCache

	mu     sync.RWMutex
	closed bool
	ds     map[core.DatastoreKey]*dsState

	openBytes atomic.Int64
	stats     counters

	stopSync chan struct{}
	syncDone chan struct{}
}

var _ core.TerrainStore = (*Store)(nil)

type counters struct {
	deltas, keyframes, recordBytes atomic.Uint64
	commits, late, accepted        atomic.Uint64
}

// dsState is one datastore.
type dsState struct {
	key    core.DatastoreKey
	id     string
	dir    string
	window core.TimeMs

	// mu guards tiles, the horizons, first/last and arrival. Apply holds it for a batch;
	// readers hold it only to look things up.
	mu sync.RWMutex
	// applyHorizon is the late threshold. A commit raises it before it starts writing,
	// so Apply never touches a window that is being committed. horizon is the published
	// horizon readers see; it is raised when the commit finished.
	applyHorizon core.TimeMs
	horizon      core.TimeMs
	first, last  core.TimeMs
	hasData      bool
	arrival      uint64
	tiles        map[core.TileCoord]*tileState

	commitMu sync.Mutex
	pal      *palette
	wal      *wal
	walRaw   []byte // WAL scratch, guarded by mu
}

// tileState is the in-memory summary of one z0 tile. History stays on disk.
type tileState struct {
	coord core.TileCoord
	name  string // series name

	mu sync.Mutex
	// Published committed state. Only commit changes it (holding dsState.commitMu).
	versions      uint32
	firstT, lastT core.TimeMs
	// Keyframe rule state.
	sinceBase      int
	bytesSinceBase int64
	refLen         int
	// Open windows: window start -> chunks. nil when there are none.
	open    map[core.TimeMs]*openWindow
	liveSeq uint64
}

type openWindow struct {
	chunks map[uint8]*openChunk
}

type openChunk struct {
	t       core.TimeMs
	arrival uint64
	deleted bool
	px      core.ChunkPixels
}

// dsMeta is meta.json.
type dsMeta struct {
	Key           core.DatastoreKey `json:"key"`
	WindowMs      int64             `json:"window_ms"`
	GridOrigin    int               `json:"grid_origin"`
	FormatVersion int               `json:"format_version"`
}

// dsPersist is state.json: the committed horizon and the update time range.
type dsPersist struct {
	Horizon *int64 `json:"horizon,omitempty"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
	HasData bool   `json:"has_data"`
}

type rootMarker struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

// Open opens (or creates) the database at dir. Terrain lives in dir/datastores.
func Open(dir string, opt Options) (*Store, error) {
	opt.defaults()
	if err := os.MkdirAll(filepath.Join(dir, "datastores"), 0o755); err != nil {
		return nil, err
	}
	if err := checkRoot(dir); err != nil {
		return nil, err
	}
	ss, err := series.Open(filepath.Join(dir, "datastores"), series.Options{
		MaxOpen: opt.MaxOpenSeries,
		NoSync:  opt.NoSync,
		OnRecover: func(name string, rep series.RecoveryReport) {
			opt.Logf("terrain: recovered series %s: %+v", name, rep)
		},
	})
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(opt.Level),
		zstd.WithEncoderConcurrency(opt.Workers),
		zstd.WithLowerEncoderMem(true),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
		zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir:    dir,
		opt:    opt,
		series: ss,
		enc:    enc,
		dec:    dec,
		cache:  newDecodedCache(max(opt.DecodedCacheBytes, 0)),
		ds:     map[core.DatastoreKey]*dsState{},
	}
	ents, err := os.ReadDir(filepath.Join(dir, "datastores"))
	if err != nil {
		s.abort()
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "datastores", e.Name(), "meta.json")); err != nil {
			continue // not a datastore (or torn creation): ignored
		}
		d, err := s.loadDS(e.Name())
		if err != nil {
			s.abort()
			return nil, fmt.Errorf("terrain: datastore %s: %w", e.Name(), err)
		}
		s.ds[d.key] = d
	}
	s.stopSync = make(chan struct{})
	s.syncDone = make(chan struct{})
	go s.syncer()
	return s, nil
}

func (s *Store) abort() {
	for _, d := range s.ds {
		if d.wal != nil {
			d.wal.close()
		}
		d.pal.close()
	}
	s.series.Close()
	s.enc.Close()
	s.dec.Close()
}

func checkRoot(dir string) error {
	p := filepath.Join(dir, "timeshadedb-v3.json")
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return writeJSON(p, rootMarker{Format: "timeshadedb-v3", Version: 1}, true)
	}
	if err != nil {
		return err
	}
	var m rootMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("terrain: %s: %w", p, err)
	}
	if m.Format != "timeshadedb-v3" || m.Version > 1 {
		return fmt.Errorf("terrain: %s: unsupported format %q version %d", p, m.Format, m.Version)
	}
	return nil
}

// writeJSON writes v to path as tmp + rename.
func writeJSON(path string, v any, sync bool) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tileName(id string, t core.TileCoord) string {
	return id + "/tiles/" + strconv.Itoa(int(t.X)) + "_" + strconv.Itoa(int(t.Y))
}

func parseTileFile(name string) (core.TileCoord, bool) {
	base, ok := strings.CutSuffix(name, ".blk")
	if !ok {
		return core.TileCoord{}, false
	}
	xs, ys, ok := strings.Cut(base, "_")
	if !ok {
		return core.TileCoord{}, false
	}
	x, err1 := strconv.Atoi(xs)
	y, err2 := strconv.Atoi(ys)
	t := core.TileCoord{X: int32(x), Y: int32(y)}
	if err1 != nil || err2 != nil || !t.Valid() || strconv.Itoa(x) != xs || strconv.Itoa(y) != ys {
		return core.TileCoord{}, false
	}
	return t, true
}

func newDS(key core.DatastoreKey, dir string, w core.TimeMs) *dsState {
	return &dsState{
		key:          key,
		id:           key.ID(),
		dir:          dir,
		window:       w,
		applyHorizon: noHorizon,
		horizon:      noHorizon,
		tiles:        map[core.TileCoord]*tileState{},
	}
}

// loadDS opens an existing datastore directory: meta, palette, state, tile summaries,
// then the WAL replay.
func (s *Store) loadDS(id string) (*dsState, error) {
	dir := filepath.Join(s.dir, "datastores", id)
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var m dsMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.FormatVersion > 1 {
		return nil, fmt.Errorf("format_version %d is newer than this server", m.FormatVersion)
	}
	if m.Key.ID() != id || m.Key.Validate() != nil || m.WindowMs <= 0 {
		return nil, fmt.Errorf("meta.json key %+v does not match directory %s", m.Key, id)
	}
	if m.GridOrigin != core.GridOriginTiles {
		return nil, fmt.Errorf("grid origin %d is not supported", m.GridOrigin)
	}
	d := newDS(m.Key, dir, core.TimeMs(m.WindowMs))
	if raw, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		var p dsPersist
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("state.json: %w", err)
		}
		if p.Horizon != nil {
			d.horizon = core.TimeMs(*p.Horizon)
			d.applyHorizon = d.horizon
		}
		d.first, d.last, d.hasData = core.TimeMs(p.First), core.TimeMs(p.Last), p.HasData
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if d.pal, err = openPalette(filepath.Join(dir, "palette.bin"), s.opt.NoSync); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "tiles"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		tc, ok := parseTileFile(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		tl := &tileState{coord: tc, name: tileName(id, tc)}
		h, err := s.series.OpenSeries(tl.name, false)
		if err != nil {
			return nil, err
		}
		sum := h.Summary()
		h.Close()
		tl.versions, tl.firstT, tl.lastT = sum.Versions, sum.FirstT, sum.LastT
		tl.sinceBase, tl.bytesSinceBase = sum.SinceBase, sum.BytesSinceBase
		tl.refLen = sum.LastBaseLen
		if tl.refLen == 0 {
			tl.refLen = sum.FirstLen
		}
		d.tiles[tc] = tl
	}
	if !s.opt.NoWAL {
		if d.wal, err = openWAL(filepath.Join(dir, "wal"), s.opt.NoSync); err != nil {
			return nil, err
		}
		err := d.wal.replay(s.dec, func(ups []walUpdate) {
			for i := range ups {
				u := &ups[i]
				tc, block := u.chunk.Tile()
				if !tc.Valid() || u.t < d.applyHorizon {
					continue
				}
				s.acceptLocked(d, tc, block, u.t, u.deleted, &u.px)
			}
		})
		if err != nil {
			return nil, fmt.Errorf("wal: %w", err)
		}
		if err := d.wal.start(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// ensureDS returns the datastore, creating it (directory, meta.json, palette, WAL) if
// needed. w <= 0 means the store default.
func (s *Store) ensureDS(key core.DatastoreKey, w core.TimeMs) (*dsState, error) {
	s.mu.RLock()
	d, closed := s.ds[key], s.closed
	s.mu.RUnlock()
	if closed {
		return nil, core.ErrClosed
	}
	if d != nil {
		return d, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, core.ErrClosed
	}
	if d := s.ds[key]; d != nil {
		return d, nil
	}
	if w <= 0 {
		w = s.opt.Window
	}
	id := key.ID()
	dir := filepath.Join(s.dir, "datastores", id)
	if err := os.MkdirAll(filepath.Join(dir, "tiles"), 0o755); err != nil {
		return nil, err
	}
	d = newDS(key, dir, w)
	var err error
	if d.pal, err = openPalette(filepath.Join(dir, "palette.bin"), s.opt.NoSync); err != nil {
		return nil, err
	}
	if !s.opt.NoWAL {
		if d.wal, err = openWAL(filepath.Join(dir, "wal"), s.opt.NoSync); err != nil {
			d.pal.close()
			return nil, err
		}
		if err := d.wal.start(); err != nil {
			d.pal.close()
			return nil, err
		}
	}
	// meta.json last: a directory without it is ignored on open.
	meta := dsMeta{Key: key, WindowMs: int64(w), GridOrigin: core.GridOriginTiles, FormatVersion: 1}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta, !s.opt.NoSync); err != nil {
		d.pal.close()
		if d.wal != nil {
			d.wal.close()
		}
		return nil, err
	}
	s.ds[key] = d
	return d, nil
}

// EnsureDatastore implements core.TerrainStore.
func (s *Store) EnsureDatastore(ctx context.Context, key core.DatastoreKey, w core.TimeMs) error {
	if err := key.Validate(); err != nil {
		return err
	}
	_, err := s.ensureDS(key, w)
	return err
}

// Apply implements core.TerrainStore.
func (s *Store) Apply(ctx context.Context, updates []core.ChunkUpdate) (core.ApplyResult, error) {
	var res core.ApplyResult
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return res, core.ErrClosed
	}
	if lim := s.opt.OpenWindowBytes; lim > 0 && s.openBytes.Load() >= lim {
		return res, ErrOverBudget
	}
	// Group valid updates by datastore, keeping their order.
	type group struct {
		d   *dsState
		ups []*core.ChunkUpdate
	}
	var groups []group
	for i := range updates {
		u := &updates[i]
		if u.DS.Validate() != nil || (!u.Deleted && u.Pixels == nil) {
			res.Invalid++
			continue
		}
		if tc, _ := u.Chunk.Tile(); !tc.Valid() {
			res.Invalid++
			continue
		}
		gi := -1
		for j := range groups {
			if groups[j].d.key == u.DS {
				gi = j
				break
			}
		}
		if gi < 0 {
			d, err := s.ensureDS(u.DS, 0)
			if err != nil {
				return res, err
			}
			groups = append(groups, group{d: d})
			gi = len(groups) - 1
		}
		groups[gi].ups = append(groups[gi].ups, u)
	}
	for _, g := range groups {
		r, err := s.applyDS(g.d, g.ups)
		res.Add(r)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// applyDS applies one datastore's updates under its lock. Late updates are not logged.
func (s *Store) applyDS(d *dsState, ups []*core.ChunkUpdate) (core.ApplyResult, error) {
	var res core.ApplyResult
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := ups[:0:0]
	maxT := noHorizon
	for _, u := range ups {
		if u.T < d.applyHorizon {
			res.Late++
			continue
		}
		keep = append(keep, u)
		maxT = max(maxT, u.T)
	}
	if len(keep) == 0 {
		s.stats.late.Add(uint64(res.Late))
		return res, nil
	}
	if d.wal != nil {
		var frame []byte
		frame, d.walRaw = encodeWALFrame(s.enc, keep, d.walRaw)
		if cap(d.walRaw) > 4<<20 {
			d.walRaw = nil // do not pin a huge scratch buffer
		}
		if err := d.wal.append(frame, maxT); err != nil {
			return res, err
		}
	}
	for _, u := range keep {
		tc, block := u.Chunk.Tile()
		s.acceptLocked(d, tc, block, u.T, u.Deleted, u.Pixels)
		res.Accepted++
	}
	s.stats.late.Add(uint64(res.Late))
	s.stats.accepted.Add(uint64(res.Accepted))
	return res, nil
}

// acceptLocked puts one update into its open window. Caller holds d.mu.
func (s *Store) acceptLocked(d *dsState, tc core.TileCoord, block uint8, t core.TimeMs, deleted bool, px *core.ChunkPixels) {
	tl := d.tiles[tc]
	if tl == nil {
		tl = &tileState{coord: tc, name: tileName(d.id, tc)}
		d.tiles[tc] = tl
	}
	d.arrival++
	ws := core.WindowStart(t, d.window)
	tl.mu.Lock()
	if tl.open == nil {
		tl.open = map[core.TimeMs]*openWindow{}
	}
	win := tl.open[ws]
	if win == nil {
		win = &openWindow{chunks: map[uint8]*openChunk{}}
		tl.open[ws] = win
	}
	oc := win.chunks[block]
	if oc == nil {
		oc = &openChunk{t: noHorizon}
		win.chunks[block] = oc
		s.openBytes.Add(openChunkBytes)
	}
	if t >= oc.t {
		oc.t, oc.arrival, oc.deleted = t, d.arrival, deleted
		if deleted {
			oc.px = core.ChunkPixels{}
		} else {
			oc.px = *px
		}
	}
	tl.liveSeq++
	tl.mu.Unlock()
	if !d.hasData || t < d.first {
		d.first = t
	}
	if !d.hasData || t > d.last {
		d.last = t
	}
	d.hasData = true
}

// Commit implements core.TerrainStore.
func (s *Store) Commit(ctx context.Context, upTo core.TimeMs) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return core.ErrClosed
	}
	dss := make([]*dsState, 0, len(s.ds))
	for _, d := range s.ds {
		dss = append(dss, d)
	}
	s.mu.RUnlock()
	sort.Slice(dss, func(i, j int) bool { return dss[i].id < dss[j].id })
	var first error
	for _, d := range dss {
		if err := s.commitDS(ctx, d, upTo); err != nil && first == nil {
			first = fmt.Errorf("terrain: commit %s: %w", d.id, err)
		}
	}
	return first
}

// closingTile is one tile's windows to close in a commit.
type closingTile struct {
	tl      *tileState
	starts  []core.TimeMs
	windows []*openWindow
}

func (s *Store) commitDS(ctx context.Context, d *dsState, upTo core.TimeMs) error {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	newH := core.WindowStart(upTo, d.window)
	d.mu.Lock()
	if newH <= d.horizon {
		d.mu.Unlock()
		return nil
	}
	if newH > d.applyHorizon {
		d.applyHorizon = newH
	}
	var work []closingTile
	for _, tl := range d.tiles {
		tl.mu.Lock()
		var ct closingTile
		for ws, win := range tl.open {
			if ws < newH {
				ct.starts = append(ct.starts, ws)
				ct.windows = append(ct.windows, win)
			}
		}
		tl.mu.Unlock()
		if len(ct.starts) > 0 {
			ct.tl = tl
			sort.Sort(byStart(ct))
			work = append(work, ct)
		}
	}
	d.mu.Unlock()

	// Palette first: new colours are durable before any block uses them.
	for _, ct := range work {
		for _, win := range ct.windows {
			for _, oc := range win.chunks {
				if err := d.pal.add(oc.px[:]); err != nil {
					return err
				}
			}
		}
	}
	if err := d.pal.flush(); err != nil {
		return err
	}

	// Encode and append per tile, in parallel.
	errs := make([]error, len(work))
	sem := make(chan struct{}, s.opt.Workers)
	var wg sync.WaitGroup
	for i := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = s.commitTile(d, &work[i])
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	d.mu.Lock()
	d.horizon = newH
	persist := dsPersist{First: int64(d.first), Last: int64(d.last), HasData: d.hasData}
	d.mu.Unlock()
	h := int64(newH)
	persist.Horizon = &h
	if err := writeJSON(filepath.Join(d.dir, "state.json"), persist, !s.opt.NoSync); err != nil {
		return err
	}
	s.stats.commits.Add(1)
	if d.wal != nil {
		if err := d.wal.rotate(newH); err != nil {
			return err
		}
	}
	return nil
}

type byStart closingTile

func (b byStart) Len() int           { return len(b.starts) }
func (b byStart) Less(i, j int) bool { return b.starts[i] < b.starts[j] }
func (b byStart) Swap(i, j int) {
	b.starts[i], b.starts[j] = b.starts[j], b.starts[i]
	b.windows[i], b.windows[j] = b.windows[j], b.windows[i]
}

// commitTile writes one tile's closed windows and publishes the result. Only the
// commit (holding d.commitMu) changes the published fields, so it reads them without
// the tile lock.
func (s *Store) commitTile(d *dsState, ct *closingTile) (err error) {
	tl := ct.tl
	h, err := s.series.OpenSeries(tl.name, true)
	if err != nil {
		return err
	}
	defer h.Close()
	cur, err := s.image(d, tl, h, core.Version(tl.versions))
	if err != nil {
		return err
	}
	pal := d.pal.lut
	versions, firstT, lastT := tl.versions, tl.firstT, tl.lastT
	sinceBase, bytesSinceBase, refLen := tl.sinceBase, tl.bytesSinceBase, tl.refLen
	owned := false
	var scratch [core.ChunkPixelCount]uint16
	var raw []byte
	written := 0 // windows fully handled

	defer func() {
		if syncErr := h.Sync(); err == nil {
			err = syncErr
		}
		tl.mu.Lock()
		tl.versions, tl.firstT, tl.lastT = versions, firstT, lastT
		tl.sinceBase, tl.bytesSinceBase, tl.refLen = sinceBase, bytesSinceBase, refLen
		removed := 0
		for i := 0; i < written; i++ {
			removed += len(ct.windows[i].chunks)
			delete(tl.open, ct.starts[i])
		}
		if len(tl.open) == 0 {
			tl.open = nil
		}
		tl.mu.Unlock()
		s.openBytes.Add(-int64(removed) * openChunkBytes)
		if owned {
			s.cache.put(cacheKey{d, tl.coord, core.Version(versions)}, cur)
		}
	}()

	for wi, ws := range ct.starts {
		end := ws + d.window
		if versions > 0 && end <= lastT {
			// Already written before a crash that lost the horizon (replay rule).
			written = wi + 1
			continue
		}
		win := ct.windows[wi]
		blocks := make([]int, 0, len(win.chunks))
		for b := range win.chunks {
			blocks = append(blocks, int(b))
		}
		sort.Ints(blocks)
		var changes []chunkChange
		for _, b := range blocks {
			if c, ok := diffChunk(uint8(b), cur, &win.chunks[uint8(b)].px, pal, scratch[:]); ok {
				changes = append(changes, c)
			}
		}
		if len(changes) == 0 {
			written = wi + 1
			continue
		}
		if !owned {
			cur = cur.Clone()
			owned = true
		}
		for _, b := range blocks {
			cur.SetBlock(uint8(b), &win.chunks[uint8(b)].px)
		}
		wide := maxIndex(changes) > 255
		raw = encodeDelta(raw[:0], changes, wide)
		payload := s.enc.EncodeAll(raw, nil)
		var flags core.RecordFlags
		if wide {
			flags = core.FlagWide
		}
		if _, err := h.Append(context.Background(), core.Record{
			RecordMeta: core.RecordMeta{TStart: ws, TLen: uint32(d.window), Kind: core.RecordDelta, Flags: flags},
			Payload:    payload,
		}); err != nil {
			return err
		}
		s.stats.deltas.Add(1)
		s.stats.recordBytes.Add(uint64(len(payload) + series.HeaderSize + series.IndexRecSize))
		versions++
		if versions == 1 {
			firstT = end
			refLen = len(payload)
		}
		lastT = end
		sinceBase++
		bytesSinceBase += int64(len(payload))
		written = wi + 1
		if versions > 1 && (sinceBase >= s.opt.MaxChain || float64(bytesSinceBase) >= s.opt.KeyframeFactor*float64(refLen)) {
			raw, wide = encodeKeyframe(raw[:0], cur, pal)
			kp := s.enc.EncodeAll(raw, nil)
			flags = 0
			if wide {
				flags = core.FlagWide
			}
			if _, err := h.Append(context.Background(), core.Record{
				RecordMeta: core.RecordMeta{TStart: ws, TLen: uint32(d.window), Kind: core.RecordKeyframe, Flags: flags},
				Payload:    kp,
			}); err != nil {
				return err
			}
			s.stats.keyframes.Add(1)
			s.stats.recordBytes.Add(uint64(len(kp) + series.HeaderSize + series.IndexRecSize))
			sinceBase, bytesSinceBase, refLen = 0, 0, len(kp)
		}
	}
	return nil
}

// syncer fsyncs the WALs every SyncInterval (group commit).
func (s *Store) syncer() {
	defer close(s.syncDone)
	t := time.NewTicker(s.opt.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopSync:
			return
		case <-t.C:
			s.mu.RLock()
			for _, d := range s.ds {
				if d.wal != nil {
					if err := d.wal.sync(); err != nil {
						s.opt.Logf("terrain: wal sync %s: %v", d.id, err)
					}
				}
			}
			s.mu.RUnlock()
		}
	}
}

// Close implements core.TerrainStore. Open windows stay in the WAL and are replayed by
// the next Open (with NoWAL they are dropped).
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	dss := s.ds
	s.mu.Unlock()
	close(s.stopSync)
	<-s.syncDone
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, d := range dss {
		// Wait for a running commit.
		d.commitMu.Lock()
		if d.wal != nil {
			keep(d.wal.close())
		}
		keep(d.pal.close())
		d.commitMu.Unlock()
	}
	keep(s.series.Close())
	s.enc.Close()
	s.dec.Close()
	s.cache.clear()
	s.openBytes.Store(0)
	return first
}

// Stats are the store's counters and memory use.
type Stats struct {
	Datastores        int
	Tiles             int
	OpenWindowBytes   int64
	DecodedCacheBytes int64
	CacheHits         uint64
	CacheMisses       uint64
	DeltasWritten     uint64
	KeyframesWritten  uint64
	RecordBytes       uint64 // payload + header + index bytes written
	Commits           uint64
	Accepted, Late    uint64
	WALBytes          int64
	Series            series.Stats
}

// Stats returns current counters.
func (s *Store) Stats() Stats {
	st := Stats{
		OpenWindowBytes:   s.openBytes.Load(),
		DecodedCacheBytes: s.cache.bytes(),
		CacheHits:         s.cache.hits.Load(),
		CacheMisses:       s.cache.misses.Load(),
		DeltasWritten:     s.stats.deltas.Load(),
		KeyframesWritten:  s.stats.keyframes.Load(),
		RecordBytes:       s.stats.recordBytes.Load(),
		Commits:           s.stats.commits.Load(),
		Accepted:          s.stats.accepted.Load(),
		Late:              s.stats.late.Load(),
		Series:            s.series.Stats(),
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st.Datastores = len(s.ds)
	for _, d := range s.ds {
		d.mu.RLock()
		st.Tiles += len(d.tiles)
		d.mu.RUnlock()
		if d.wal != nil {
			st.WALBytes += d.wal.size()
		}
	}
	return st
}

// DropCaches empties the decoded-tile cache (benchmarks: cold reads).
func (s *Store) DropCaches() { s.cache.clear() }

// SeriesStore returns the underlying series store (rooted at <db>/datastores), which
// the pyramid (WP3) can share for its coarse series.
func (s *Store) SeriesStore() *series.Store { return s.series }
