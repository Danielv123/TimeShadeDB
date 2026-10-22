// Package terrain is the z0 terrain store of TimeShadeDB v3 (PLAN.md §4.2, WP2). It
// implements core.TerrainStore on top of the series package:
//
//   - one series per (datastore, z0 tile) under <db>/datastores/<id>/tiles/<x>_<y>;
//   - a delta record per tile and closed window with the net change of that window, in
//     palette indices (FORMAT.md 5.3), and keyframes chosen by a byte rule;
//   - an append-only palette per datastore;
//   - open windows in memory (the newest update per chunk and window) backed by a
//     write-ahead log, bounded by a byte budget beyond which whole windows are spilled
//     to disk (spill.go);
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
	"slices"
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

// Encoder limits: concurrent encoder slots and the zstd window (a keyframe is 256 KiB).
const (
	maxEncoders   = 4
	maxDecoders   = 8
	encoderWindow = 256 << 10
)

// walBufs recycles WAL frame buffers: the raw scratch (about 2 KiB per update, at most
// walFrameUpdates updates) and the encoded frames of a batch, which the WAL writes
// before Apply returns. A short list rather than a sync.Pool: under ingest load the
// collector runs often and a Pool drops its buffers, so every batch grew new ones.
var walBufs = make(chan []byte, 3)

func getWALBuf() []byte {
	select {
	case b := <-walBufs:
		return b[:0]
	default:
		return nil
	}
}

func putWALBuf(b []byte) {
	if cap(b) == 0 || cap(b) > 2*walMaxPayload {
		return
	}
	select {
	case walBufs <- b:
	default:
	}
}

// decoderMaxMemory caps one decoded zstd frame: a terrain record (a keyframe is at most
// 512 KiB) or a WAL frame (walMaxRaw, about 2.1 MB).
const decoderMaxMemory = 64 << 20

// openChunkBytes is the accounted size of one open-window chunk entry.
const openChunkBytes = core.ChunkPixelCount*2 + 64

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func crcC(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// Options configure a Store. Zero values select the defaults.
type Options struct {
	// Window is the window length for new datastores (default core.DefaultTerrainWindow).
	Window core.TimeMs
	// DecodedCacheBytes bounds the decoded-tile LRU (default 64 MiB; < 0 disables it).
	DecodedCacheBytes int64
	// OpenWindowBytes is the budget for open-window buffers (default 64 MiB; < 0 means
	// unlimited). Apply counts every valid update of a batch as a new open chunk (the
	// worst case) and, when that would go over the budget, first spills whole open
	// windows to disk, oldest first, until it fits (spill.go). It never refuses a batch
	// for memory; the budget can be exceeded by at most one batch (callers keep batches
	// small, WP6: at most a few thousand updates) plus what cannot be spilled (windows a
	// commit is closing).
	OpenWindowBytes int64
	// MaxOpenSeries bounds the idle series (indexes and file handles) kept open
	// (default 256).
	MaxOpenSeries int
	// MaxIndexBytes bounds the memory of idle series indexes (default 32 MiB).
	MaxIndexBytes int64
	// SyncInterval is the WAL group-commit interval (default 1 s).
	SyncInterval time.Duration
	// NoSync skips every fsync (tests, bulk imports that can be redone).
	NoSync bool
	// NoWAL disables the write-ahead log: open windows are lost on a crash or Close.
	NoWAL bool
	// Level is the zstd level for terrain records (default zstd.SpeedDefault).
	Level zstd.EncoderLevel
	// KeyframeFactor: a keyframe is written when the delta bytes since the last base
	// reach KeyframeFactor times its size (default 4, tuned on the test-data replay;
	// FORMAT.md 5.3).
	KeyframeFactor float64
	// MaxChain caps the records between keyframes (default 256).
	MaxChain int
	// Workers is the number of tiles a commit encodes in parallel (default GOMAXPROCS).
	Workers int
	// MinTime, if positive, is the earliest update time accepted: earlier ones are
	// impossible (a broken clock) and Invalid. serve sets DefaultMinTime.
	MinTime core.TimeMs
	// Logf receives recovery notes (default: discard).
	Logf func(format string, args ...any)
}

// DefaultMinTime is the default Options.MinTime: 2016-01-01T00:00:00Z, before any
// Factorio map that can feed this store existed.
const DefaultMinTime core.TimeMs = 1_451_606_400_000

// SplitBudget returns options with a total memory budget split between the decoded
// cache (half), open windows (a quarter) and idle series indexes (an eighth). The rest
// covers encoders, the palettes and per-tile summaries. Callers that also run an
// encoded-response cache should pass only this store's share.
func SplitBudget(total int64) Options {
	return Options{
		DecodedCacheBytes: total / 2,
		OpenWindowBytes:   total / 4,
		MaxIndexBytes:     total / 8,
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
		o.KeyframeFactor = 4
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
	enc    *zstd.Encoder // terrain records
	walEnc *zstd.Encoder // WAL frames
	dec    *zstd.Decoder
	cache  *decodedCache

	mu     sync.RWMutex
	closed bool
	ds     map[core.DatastoreKey]*dsState

	openBytes atomic.Int64
	reserved  atomic.Int64 // worst-case bytes of batches being applied
	spillMu   sync.Mutex   // one spill at a time
	stats     counters

	stopSync chan struct{}
	syncDone chan struct{}
}

var _ core.TerrainStore = (*Store)(nil)

type counters struct {
	deltas, keyframes, recordBytes atomic.Uint64
	commits, late, accepted        atomic.Uint64
	spills, spilledBytes           atomic.Uint64
}

// dsState is one datastore.
type dsState struct {
	key    core.DatastoreKey
	id     string
	dir    string
	window core.TimeMs

	// mu guards tiles, the horizons, first/last and arrival. Apply holds it for the WAL
	// write and the in-memory update of a batch (palette growth and frame encoding happen
	// before); readers hold it only to look things up.
	mu sync.RWMutex
	// applyHorizon is the late threshold. A commit raises it before it starts writing
	// (and records it in state.json as "committing"), so Apply never touches a window
	// that is being committed, even across a restart. horizon is the published horizon
	// readers see; it is raised when the commit finished.
	applyHorizon core.TimeMs
	horizon      core.TimeMs
	first, last  core.TimeMs
	hasData      bool
	arrival      uint64
	tiles        map[core.TileCoord]*tileState
	// spills are the windows spilled to disk (spill.go), by start.
	spills map[core.TimeMs]*spillWin

	commitMu sync.Mutex
	pal      *palette
	wal      *wal
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
	// spilled counts the spilled windows that hold chunks of this tile (spill.go);
	// spillGen counts the spills that moved chunks of it out of memory.
	spilled  int
	spillGen uint64
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

// dsPersist is state.json: the committed horizon, the late threshold of a commit that
// has not finished, and the update time range.
type dsPersist struct {
	Horizon *int64 `json:"horizon,omitempty"`
	// Committing is set while a commit to that horizon is running or was interrupted:
	// some tiles may already hold its records. Open finishes it before accepting updates.
	Committing *int64 `json:"committing,omitempty"`
	First      int64  `json:"first"`
	Last       int64  `json:"last"`
	HasData    bool   `json:"has_data"`
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
		MaxOpen:           opt.MaxOpenSeries,
		MaxIdleIndexBytes: opt.MaxIndexBytes,
		NoSync:            opt.NoSync,
		OnRecover: func(name string, rep series.RecoveryReport) {
			opt.Logf("terrain: recovered series %s: %+v", name, rep)
		},
	})
	if err != nil {
		return nil, err
	}
	// One encoder for records and one (fastest) for the WAL, each with a few concurrent
	// slots and a 256 KiB window: the encoders' memory is bounded and small, unlike a
	// pool of NumCPU full-size encoders (PLAN.md §4.3 G5).
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(opt.Level),
		zstd.WithEncoderConcurrency(min(opt.Workers, maxEncoders)),
		zstd.WithLowerEncoderMem(true),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(encoderWindow))
	if err != nil {
		return nil, err
	}
	walEnc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(maxEncoders),
		zstd.WithLowerEncoderMem(true),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(encoderWindow))
	if err != nil {
		enc.Close()
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(min(runtime.GOMAXPROCS(0), maxDecoders)),
		zstd.WithDecoderMaxMemory(decoderMaxMemory))
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir:    dir,
		opt:    opt,
		series: ss,
		enc:    enc,
		walEnc: walEnc,
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
	s.walEnc.Close()
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
		spills:       map[core.TimeMs]*spillWin{},
	}
}

// loadDS opens an existing datastore directory: meta, palette, state, tile summaries,
// then the WAL replay.
func (s *Store) loadDS(id string) (d *dsState, err error) {
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
	d = newDS(m.Key, dir, core.TimeMs(m.WindowMs))
	pending := noHorizon // horizon of an unfinished commit
	if raw, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		var p dsPersist
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("state.json: %w", err)
		}
		if p.Horizon != nil {
			d.horizon = core.TimeMs(*p.Horizon)
			d.applyHorizon = d.horizon
		}
		if p.Committing != nil {
			pending = core.TimeMs(*p.Committing)
		}
		d.first, d.last, d.hasData = core.TimeMs(p.First), core.TimeMs(p.Last), p.HasData
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := clearSpills(dir); err != nil {
		return nil, err
	}
	if d.pal, err = openPalette(filepath.Join(dir, "palette.bin"), s.opt.NoSync); err != nil {
		return nil, err
	}
	defer func(d *dsState) { // the returned d is nil on error, so pass it in
		if err != nil {
			if d.wal != nil {
				d.wal.close()
			}
			d.pal.close()
		}
	}(d)
	ents, err := os.ReadDir(filepath.Join(dir, "tiles"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var tls []*tileState
	for _, e := range ents {
		tc, ok := parseTileFile(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		tls = append(tls, &tileState{coord: tc, name: tileName(id, tc)})
	}
	if err := s.loadTileSummaries(tls); err != nil {
		return nil, err
	}
	for _, tl := range tls {
		d.tiles[tl.coord] = tl
	}
	if !s.opt.NoWAL {
		if d.wal, err = openWAL(filepath.Join(dir, "wal"), s.opt.NoSync); err != nil {
			return nil, err
		}
		var palErr, spillErr error
		lim := s.opt.OpenWindowBytes
		err := d.wal.replay(s.dec, s.opt.Logf, func(ups []walUpdate) {
			for i := range ups {
				u := &ups[i]
				tc, block := u.chunk.Tile()
				if !tc.Valid() || u.t < d.applyHorizon {
					continue
				}
				if err := d.pal.ensure(u.px[:]); err != nil {
					palErr = err
					continue
				}
				s.acceptLocked(d, tc, block, u.t, u.deleted, &u.px)
			}
			// The replay keeps the newest windows in memory and spills the oldest.
			if lim > 0 && s.openBytes.Load() > lim && spillErr == nil {
				spillErr = s.spill(lim*3/4, d)
			}
		})
		if err == nil {
			err = palErr
		}
		if err == nil {
			err = spillErr
		}
		if err != nil {
			return nil, fmt.Errorf("wal: %w", err)
		}
		if err := d.wal.start(); err != nil {
			return nil, err
		}
	}
	if pending > d.horizon {
		// A commit to pending was interrupted after it may have written some tiles.
		// Finish it now, before any Apply: an update for a window it closes would be
		// dropped by the replay skip rule on the tiles already written but kept on the
		// others. The replayed windows hold exactly what the interrupted commit saw,
		// because Apply refused updates below pending while it ran.
		s.opt.Logf("terrain: %s: finishing the interrupted commit to %d", id, pending)
		if err := s.commitDS(context.Background(), d, pending); err != nil {
			return nil, fmt.Errorf("finishing the interrupted commit: %w", err)
		}
	}
	return d, nil
}

// loadTileWorkers bounds the tile series loadTileSummaries opens at once.
const loadTileWorkers = 16

// loadTileSummaries fills each tile's version summary from its series. Opening a series
// mostly waits on the file system (and is much slower on Windows while the files are
// fresh from a bulk write), so up to loadTileWorkers series are opened at once.
func (s *Store) loadTileSummaries(tls []*tileState) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	jobs := make(chan *tileState)
	for w := 0; w < min(loadTileWorkers, len(tls)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tl := range jobs {
				h, err := s.series.OpenSeries(tl.name, false)
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					continue
				}
				sum := h.Summary()
				h.Close()
				tl.versions, tl.firstT, tl.lastT = sum.Versions, sum.FirstT, sum.LastT
				tl.sinceBase, tl.bytesSinceBase = sum.SinceBase, sum.BytesSinceBase
				tl.refLen = sum.LastBaseLen
				if tl.refLen == 0 {
					tl.refLen = sum.FirstLen
				}
			}
		}()
	}
	for _, tl := range tls {
		mu.Lock()
		failed := first != nil
		mu.Unlock()
		if failed {
			break
		}
		jobs <- tl
	}
	close(jobs)
	wg.Wait()
	return first
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
	valid := make([]*core.ChunkUpdate, 0, len(updates))
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
		if s.opt.MinTime > 0 && u.T < s.opt.MinTime {
			res.Invalid++
			continue
		}
		valid = append(valid, u)
	}
	if len(valid) == 0 {
		return res, nil
	}
	// Budget: count every update as a new open chunk (the worst case) and reserve that
	// until the batch is applied, so concurrent batches cannot overshoot together. Over
	// the budget, spill the oldest open windows to disk first (to three quarters of it,
	// so the next batches do not spill again at once).
	if lim := s.opt.OpenWindowBytes; lim > 0 {
		need := int64(len(valid)) * openChunkBytes
		used := s.reserved.Add(need)
		defer s.reserved.Add(-need)
		if s.openBytes.Load()+used > lim {
			if err := s.spill(lim*3/4-used, nil); err != nil {
				return core.ApplyResult{}, err
			}
		}
	}
	// Group by datastore, keeping their order.
	type group struct {
		d   *dsState
		ups []*core.ChunkUpdate
	}
	var groups []group
	for _, u := range valid {
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

// notLate returns the updates at or after the late threshold h.
func notLate(ups []*core.ChunkUpdate, h core.TimeMs) []*core.ChunkUpdate {
	keep := make([]*core.ChunkUpdate, 0, len(ups))
	for _, u := range ups {
		if u.T >= h {
			keep = append(keep, u)
		}
	}
	return keep
}

// walBatch encodes the WAL frames of ups (at most walFrameUpdates updates per frame)
// into one buffer and returns it with the newest update time.
func (s *Store) walBatch(ups []*core.ChunkUpdate) ([]byte, core.TimeMs) {
	maxT := noHorizon
	for _, u := range ups {
		maxT = max(maxT, u.T)
	}
	raw, out := getWALBuf(), getWALBuf()
	for len(ups) > 0 {
		n := min(len(ups), walFrameUpdates)
		out, raw = appendWALFrame(out, s.walEnc, ups[:n], raw)
		ups = ups[n:]
	}
	putWALBuf(raw)
	return out, maxT
}

// applyDS applies one datastore's updates. Late updates are not logged. Work that
// needs no ordering (new palette colours, WAL frame encoding) is done before taking
// d.mu, so readers of the datastore wait only for the WAL write and the in-memory
// update.
func (s *Store) applyDS(d *dsState, ups []*core.ChunkUpdate) (core.ApplyResult, error) {
	var res core.ApplyResult
	d.mu.RLock()
	h := d.applyHorizon
	d.mu.RUnlock()
	keep := notLate(ups, h)
	for _, u := range keep {
		if !u.Deleted {
			if err := d.pal.ensure(u.Pixels[:]); err != nil {
				return res, err
			}
		}
	}
	var frames []byte
	var maxT core.TimeMs
	if d.wal != nil && len(keep) > 0 {
		frames, maxT = s.walBatch(keep)
	}
	defer func() { putWALBuf(frames) }() // written (or failed) by then
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.applyHorizon != h {
		// A commit raised the late threshold meanwhile (rare): filter and encode again.
		if again := notLate(keep, d.applyHorizon); len(again) != len(keep) {
			keep = again
			if d.wal != nil && len(keep) > 0 {
				putWALBuf(frames)
				frames, maxT = s.walBatch(keep)
			}
		}
	}
	res.Late = len(ups) - len(keep)
	s.stats.late.Add(uint64(res.Late))
	if len(keep) == 0 {
		return res, nil
	}
	if d.wal != nil {
		if err := d.wal.append(frames, maxT); err != nil {
			return res, err
		}
	}
	for _, u := range keep {
		tc, block := u.Chunk.Tile()
		s.acceptLocked(d, tc, block, u.T, u.Deleted, u.Pixels)
		res.Accepted++
	}
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
	return s.CommitBy(ctx, func(core.DatastoreKey) core.TimeMs { return upTo })
}

// CommitBy is Commit with a target per datastore: a server that holds the horizon of a
// map back while one of its sources still sends older data passes a lower target for
// that map's datastores (their open windows wait, spilled to disk if memory runs short).
func (s *Store) CommitBy(ctx context.Context, upTo func(core.DatastoreKey) core.TimeMs) error {
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
		if err := s.commitDS(ctx, d, upTo(d.key)); err != nil && first == nil {
			first = fmt.Errorf("terrain: commit %s: %w", d.id, err)
		}
	}
	return first
}

// closingTile is one tile's windows to close in a commit: per window start the
// in-memory window (nil when it has none) and the spilled window (nil when it was not
// spilled), merged by commitTile.
type closingTile struct {
	tl      *tileState
	starts  []core.TimeMs
	windows []*openWindow
	spills  []*spillWin
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
	var closedSpills []core.TimeMs
	for ws := range d.spills {
		if ws < newH {
			closedSpills = append(closedSpills, ws)
		}
	}
	for _, tl := range d.tiles {
		tl.mu.Lock()
		var ct closingTile
		for ws, win := range tl.open {
			if ws < newH {
				ct.starts = append(ct.starts, ws)
				ct.windows = append(ct.windows, win)
				ct.spills = append(ct.spills, nil)
			}
		}
		if tl.spilled > 0 {
			for _, ws := range closedSpills {
				sp := d.spills[ws]
				if len(sp.frames[tl.coord]) == 0 {
					continue
				}
				if i := slices.Index(ct.starts, ws); i >= 0 {
					ct.spills[i] = sp
				} else {
					ct.starts = append(ct.starts, ws)
					ct.windows = append(ct.windows, nil)
					ct.spills = append(ct.spills, sp)
				}
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

	if len(work) > 0 {
		// Record the target horizon before any tile is written, so that a restart
		// finishes this commit before it accepts updates again (loadDS).
		if err := s.writeState(d); err != nil {
			return err
		}
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
	d.mu.Unlock()
	if err := s.writeState(d); err != nil {
		return err
	}
	s.stats.commits.Add(1)
	if len(closedSpills) > 0 {
		d.mu.Lock()
		for _, ws := range closedSpills {
			if err := os.Remove(d.spills[ws].path); err != nil && !errors.Is(err, os.ErrNotExist) {
				// A live read may still have it open (Windows); Open deletes it.
				s.opt.Logf("terrain: removing spill %s: %v", d.spills[ws].path, err)
			}
			delete(d.spills, ws)
		}
		d.mu.Unlock()
	}
	if d.wal != nil {
		if err := d.wal.rotate(newH); err != nil {
			return err
		}
	}
	return nil
}

// writeState writes state.json from the datastore's current state. Only commitDS
// (holding d.commitMu) and loadDS call it.
func (s *Store) writeState(d *dsState) error {
	d.mu.RLock()
	p := dsPersist{First: int64(d.first), Last: int64(d.last), HasData: d.hasData}
	horizon, apply := d.horizon, d.applyHorizon
	d.mu.RUnlock()
	if horizon != noHorizon {
		h := int64(horizon)
		p.Horizon = &h
	}
	if apply > horizon {
		c := int64(apply)
		p.Committing = &c
	}
	return writeJSON(filepath.Join(d.dir, "state.json"), p, !s.opt.NoSync)
}

type byStart closingTile

func (b byStart) Len() int           { return len(b.starts) }
func (b byStart) Less(i, j int) bool { return b.starts[i] < b.starts[j] }
func (b byStart) Swap(i, j int) {
	b.starts[i], b.starts[j] = b.starts[j], b.starts[i]
	b.windows[i], b.windows[j] = b.windows[j], b.windows[i]
	b.spills[i], b.spills[j] = b.spills[j], b.spills[i]
}

// commitTile writes one tile's closed windows and publishes the result. Only the
// commit (holding d.commitMu) changes the published fields, so it reads them without
// the tile lock.
func (s *Store) commitTile(d *dsState, ct *closingTile) (err error) {
	tl := ct.tl
	if commitTileHook != nil {
		if err := commitTileHook(tl.name); err != nil {
			return err
		}
	}
	h, err := s.series.OpenSeries(tl.name, true)
	if err != nil {
		return err
	}
	defer h.Close()
	cur, err := s.image(d, tl, h, core.Version(tl.versions))
	if err != nil {
		return err
	}
	// Every colour of the open windows is in the palette: Apply published it before it
	// accepted the update, and this commit collected the windows after that (under
	// d.mu). The snapshot is immutable, so no lock is held while encoding.
	pal := d.pal.state().lut
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
			if ct.windows[i] != nil {
				removed += len(ct.windows[i].chunks)
				delete(tl.open, ct.starts[i])
			}
			if ct.spills[i] != nil {
				tl.spilled--
			}
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
		if sp := ct.spills[wi]; sp != nil {
			if win, err = s.mergedWindow(sp, tl.coord, win); err != nil {
				return err
			}
		}
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

// commitTileHook, if set (tests only), runs before each tile of a commit; an error
// aborts that tile as an I/O error would.
var commitTileHook func(name string) error

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
	s.walEnc.Close()
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
	// Spills counts open windows written to spill files, SpilledBytes their accounted
	// memory; SpilledWindows is the open windows spilled now.
	Spills, SpilledBytes uint64
	SpilledWindows       int
	Series               series.Stats
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
		Spills:            s.stats.spills.Load(),
		SpilledBytes:      s.stats.spilledBytes.Load(),
		Series:            s.series.Stats(),
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st.Datastores = len(s.ds)
	for _, d := range s.ds {
		d.mu.RLock()
		st.Tiles += len(d.tiles)
		st.SpilledWindows += len(d.spills)
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
