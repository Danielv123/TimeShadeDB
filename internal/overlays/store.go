// Package overlays is the on-disk core.OverlayStore of TimeShadeDB v3
// (docs/FORMAT.md 5.6): chart tags, machine recipes and rail entities as intervals
// [Start, End), stored per datastore in an append-only log with a per-tile index.
//
// Layout under <db>/overlays/<datastore id>/:
//
//	meta.json   {"key":{...},"format_version":1}
//	log.ivl     the interval log
//
// The log is a sequence of records, each "u32 payload length | u32 CRC-32C | payload":
//
//	open:  0x01, uvarint ordinal, varint start, kind byte (1 tag, 2 recipe, 3 rail),
//	       string key, varint instance, f64 x, f64 y, strings icon, text, recipe,
//	       quality, name, byte direction
//	end:   0x02, uvarint ordinal of the open record it ends, varint end
//
// Ordinals number the open records of the log from 0. Strings are a uvarint length and
// bytes. One Apply writes one buffer per datastore with one write call (and one fsync),
// with every end record before the open record that replaces it, so a torn write never
// leaves two open intervals for one key.
//
// The per-tile index is rebuilt from the log at Open (O(records)) and kept in memory:
// every record belongs to the z0 tile of the interval's position (an end record to the
// tile of the interval it ends), and the index holds each tile's record locations,
// 8 bytes per record. The tile Version is its record count. Open intervals cost one
// entry each (tile, ordinal, start and a 64-bit content hash, about 90 bytes with the
// key). Interval contents stay on disk; queries read a tile's records with positioned
// reads through an LRU of decoded tiles bounded by Options.CacheBytes.
//
// At Open a torn tail (an incomplete or bad frame with no valid record after it, as a
// crash during the last write leaves) is truncated. Damage followed by valid records
// is not a torn write: deleting those records would lose history and let tile versions
// (cache and ETag keys) go back and be reused for other content, so Open fails with
// ErrCorrupt instead. With Options.Salvage the damaged log is kept as
// log.ivl.damaged-<unix ns> and the store continues from the valid prefix.
//
// Applying events creates a datastore's directory only when it gets its first record,
// so close events for unknown keys or datastores leave no state behind. Every
// datastore keeps its log open; the per-tile index (8 bytes per record) and the open
// map (about 140 bytes per open interval, key included) are in memory and are not part
// of a memory budget, and the log is never compacted.
package overlays

import (
	"container/list"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"timeshadedb/internal/core"
)

// FormatVersion is the overlays meta.json format_version.
const FormatVersion = 1

// MaxTextBytes bounds the text fields of one interval (icon, text, recipe, quality and
// name together); larger open events are Invalid. Keys are at most 1,024 bytes.
const MaxTextBytes = 16 << 10

// Options tunes a Store.
type Options struct {
	// CacheBytes bounds the decoded-tile cache (default 32 MiB).
	CacheBytes int64
	// NoSync skips the fsync after each Apply (tests, bulk imports).
	NoSync bool
	// Salvage opens a log with damage in the middle: the damaged file is kept as
	// log.ivl.damaged-<unix ns> and the store continues from the records before the
	// damage (Stats.Salvaged). Without it Open fails with ErrCorrupt.
	Salvage bool
}

// Store implements core.OverlayStore on disk.
type Store struct {
	opts Options
	dir  string

	mu     sync.Mutex
	closed bool
	err    error // sticky: a failed log write leaves memory ahead of disk
	ds     map[core.DatastoreKey]*dsState

	cache      map[cacheKey]*list.Element
	lru        *list.List
	cacheBytes int64
	stats      Stats
}

var _ core.OverlayStore = (*Store)(nil)

// ErrCorrupt is returned by Open for a log with damage followed by valid records.
var ErrCorrupt = errors.New("overlays: log damaged before its end")

// Stats are counters and gauges for /metrics and tests.
type Stats struct {
	Accepted, Late, Invalid int64
	Records                 int64 // records appended since Open
	CacheBytes              int64 // gauge
	CacheHits, CacheMisses  int64
	OpenIntervals           int   // gauge
	Tiles                   int   // gauge: tiles with overlays
	LogBytes                int64 // gauge: total log size
	TornBytes               int64 // torn tails truncated at Open
	Salvaged                int   // damaged logs set aside at Open (Options.Salvage)
	// IndexBytes (gauge) estimates the memory of what Open rebuilds and keeps for
	// every datastore: the per-tile record index (8 B per record plus about 64 B per
	// tile) and the open-interval map (about 100 B plus the key per open interval).
	// It grows with the history and is not part of CacheBytes or any budget.
	IndexBytes int64
}

type kindKey struct {
	kind core.IntervalKind
	key  string
}

type openRef struct {
	tile  core.TileCoord
	ord   uint32
	start core.TimeMs
	hash  uint64
}

// loc packs a record's log offset (40 bits) and frame length (24 bits).
type loc uint64

func mkLoc(off int64, n int) loc { return loc(uint64(off)<<24 | uint64(n)) }
func (l loc) off() int64         { return int64(l >> 24) }
func (l loc) len() int           { return int(l & (1<<24 - 1)) }

type dsState struct {
	key   core.DatastoreKey
	dir   string
	f     *os.File // the log, open for reading and writing
	size  int64
	opens uint32 // next ordinal
	open  map[kindKey]openRef
	tiles map[core.TileCoord][]loc
}

type cacheKey struct {
	ds   *dsState
	tile core.TileCoord
}

type cacheEntry struct {
	key       cacheKey
	version   int
	intervals []core.Interval
	bytes     int64
}

// Open opens (or creates) the overlay store under <db>/overlays.
func Open(db string, opts Options) (*Store, error) {
	if opts.CacheBytes <= 0 {
		opts.CacheBytes = 32 << 20
	}
	o := &Store{opts: opts, dir: filepath.Join(db, "overlays"), ds: map[core.DatastoreKey]*dsState{},
		cache: map[cacheKey]*list.Element{}, lru: list.New()}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		key, err := core.ParseDatastoreID(e.Name())
		if err != nil {
			continue // unknown files are ignored
		}
		d, err := o.loadDS(key)
		if err != nil {
			o.closeFiles()
			return nil, fmt.Errorf("overlays: %s: %w", e.Name(), err)
		}
		if d != nil {
			o.ds[key] = d
		}
	}
	return o, nil
}

type dsMeta struct {
	Key           core.DatastoreKey `json:"key"`
	FormatVersion int               `json:"format_version"`
}

func (o *Store) newDS(key core.DatastoreKey) *dsState {
	return &dsState{key: key, dir: filepath.Join(o.dir, key.ID()), open: map[kindKey]openRef{}, tiles: map[core.TileCoord][]loc{}}
}

func (d *dsState) logPath() string { return filepath.Join(d.dir, "log.ivl") }

// loadDS reads meta.json and replays the log, truncating a torn tail; damage followed by
// valid records is ErrCorrupt (or salvaged).
func (o *Store) loadDS(key core.DatastoreKey) (*dsState, error) {
	d := o.newDS(key)
	b, err := os.ReadFile(filepath.Join(d.dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m dsMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("meta.json: %w", err)
	}
	if m.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("format_version %d is newer than %d", m.FormatVersion, FormatVersion)
	}
	if m.Key != key {
		return nil, fmt.Errorf("meta.json key %v does not match the directory", m.Key)
	}
	log, err := os.ReadFile(d.logPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// The tile and the open map entry of every open record, by ordinal.
	var ordTile []core.TileCoord
	var ordKey []kindKey
	off := 0
	good := scanRecords(log, func(rec []byte) bool {
		r, err := decodeRecord(rec)
		if err != nil {
			return false
		}
		var tile core.TileCoord
		if r.end {
			if r.ord >= uint64(len(ordTile)) {
				return false
			}
			k := ordKey[r.ord]
			if cur, ok := d.open[k]; !ok || uint64(cur.ord) != r.ord {
				return false // ends an interval that is not open
			}
			delete(d.open, k)
			tile = ordTile[r.ord]
		} else {
			if r.ord != uint64(len(ordTile)) {
				return false
			}
			tile = core.TileAt(r.iv.X, r.iv.Y, 0)
			k := kindKey{r.iv.Kind, r.iv.Key}
			if !tile.Valid() {
				return false
			}
			if _, ok := d.open[k]; ok {
				return false // a second open interval for the key
			}
			ordTile = append(ordTile, tile)
			ordKey = append(ordKey, k)
			d.open[k] = openRef{tile: tile, ord: uint32(r.ord), start: r.iv.Start, hash: contentHash(&r.iv)}
		}
		d.tiles[tile] = append(d.tiles[tile], mkLoc(int64(off), 8+len(rec)))
		off += 8 + len(rec)
		return true
	})
	d.opens = uint32(len(ordTile))
	if good < len(log) && validFrameAfter(log, good) {
		if !o.opts.Salvage {
			return nil, fmt.Errorf("%w: %s at offset %d of %d (Options.Salvage keeps the %d bytes before it)",
				ErrCorrupt, d.logPath(), good, len(log), good)
		}
		aside := fmt.Sprintf("%s.damaged-%d", d.logPath(), time.Now().UnixNano())
		if err := os.Rename(d.logPath(), aside); err != nil {
			return nil, err
		}
		if err := os.WriteFile(d.logPath(), log[:good], 0o644); err != nil {
			return nil, err
		}
		o.stats.Salvaged++
	} else if good < len(log) {
		o.stats.TornBytes += int64(len(log) - good)
	}
	f, err := os.OpenFile(d.logPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if good < len(log) {
		if err := f.Truncate(int64(good)); err != nil {
			f.Close()
			return nil, err
		}
		if !o.opts.NoSync {
			if err := f.Sync(); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	d.f, d.size = f, int64(good)
	return d, nil
}

// contentHash hashes every interval field except Start and End (core.Interval.SameContent).
func contentHash(iv *core.Interval) uint64 {
	h := fnv.New64a()
	h.Write(appendContent(nil, iv))
	return h.Sum64()
}

// Apply implements core.OverlayStore.
func (o *Store) Apply(ctx context.Context, events []core.OverlayEvent) (core.ApplyResult, error) {
	var res core.ApplyResult
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return res, core.ErrClosed
	}
	if o.err != nil {
		return res, o.err
	}
	type pending struct {
		buf   []byte
		tiles map[core.TileCoord][]loc
	}
	writes := map[*dsState]*pending{}
	var order []*dsState
	add := func(d *dsState, tile core.TileCoord, rec []byte) {
		p := writes[d]
		if p == nil {
			p = &pending{tiles: map[core.TileCoord][]loc{}}
			writes[d] = p
			order = append(order, d)
		}
		p.tiles[tile] = append(p.tiles[tile], mkLoc(d.size+int64(len(p.buf)), 8+len(rec)))
		p.buf = appendFrame(p.buf, rec)
		o.stats.Records++
	}
	var created []*dsState
	for i := range events {
		ev := &events[i]
		iv := ev.Interval
		if ev.DS.Validate() != nil || !iv.Kind.Valid() || iv.Key == "" || len(iv.Key) > 1024 {
			res.Invalid++
			continue
		}
		var tile core.TileCoord
		if !ev.Close {
			if !finite(iv.X) || !finite(iv.Y) || len(iv.Icon)+len(iv.Text)+len(iv.Recipe)+len(iv.Quality)+len(iv.Name) > MaxTextBytes {
				res.Invalid++
				continue
			}
			if tile = core.TileAt(iv.X, iv.Y, 0); !tile.Valid() {
				res.Invalid++
				continue
			}
		}
		d := o.ds[ev.DS]
		if d == nil {
			d = o.newDS(ev.DS)
			o.ds[ev.DS] = d
			created = append(created, d)
		}
		k := kindKey{iv.Kind, iv.Key}
		cur, hasCur := d.open[k]
		if hasCur && ev.T < cur.start {
			res.Late++
			continue
		}
		res.Accepted++
		var hash uint64
		if !ev.Close {
			n := iv
			n.Start, n.End = 0, 0
			hash = contentHash(&n)
			if hasCur && cur.hash == hash {
				continue // same content: nothing happens
			}
		}
		if hasCur {
			add(d, cur.tile, encodeEnd(cur.ord, ev.T))
			delete(d.open, k)
		}
		if ev.Close {
			continue
		}
		n := iv
		n.Start, n.End = ev.T, 0
		add(d, tile, encodeOpen(d.opens, &n))
		d.open[k] = openRef{tile: tile, ord: d.opens, start: ev.T, hash: hash}
		d.opens++
	}
	o.stats.Accepted += int64(res.Accepted)
	o.stats.Late += int64(res.Late)
	o.stats.Invalid += int64(res.Invalid)
	for _, d := range created {
		if writes[d] == nil {
			delete(o.ds, d.key) // nothing to store: leave no state behind
			continue
		}
		if err := o.create(d); err != nil {
			o.err = fmt.Errorf("overlays: %w", err)
			return res, o.err
		}
	}
	for _, d := range order {
		p := writes[d]
		if d.size+int64(len(p.buf)) >= 1<<40 {
			o.err = fmt.Errorf("overlays: %s: log would exceed 1 TiB", d.dir)
			return res, o.err
		}
		if _, err := d.f.WriteAt(p.buf, d.size); err != nil {
			o.err = fmt.Errorf("overlays: %w", err)
			return res, o.err
		}
		if !o.opts.NoSync {
			if err := d.f.Sync(); err != nil {
				o.err = fmt.Errorf("overlays: %w", err)
				return res, o.err
			}
		}
		d.size += int64(len(p.buf))
		for tile, locs := range p.tiles {
			d.tiles[tile] = append(d.tiles[tile], locs...)
		}
	}
	return res, nil
}

func finite(v float64) bool { return v-v == 0 }

// create writes meta.json and opens the log of a new datastore.
func (o *Store) create(d *dsState) error {
	b, err := json.Marshal(dsMeta{Key: d.key, FormatVersion: FormatVersion})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(d.dir, "meta.json.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil && !o.opts.NoSync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(d.dir, "meta.json")); err != nil {
		return err
	}
	d.f, err = os.OpenFile(d.logPath(), os.O_CREATE|os.O_RDWR, 0o644)
	return err
}

// Query implements core.OverlayStore.
func (o *Store) Query(ctx context.Context, ds core.DatastoreKey, q core.OverlayQuery) ([]core.Interval, error) {
	if q.Tile.Z != 0 || !q.Tile.Valid() {
		return nil, fmt.Errorf("%w: %v", core.ErrBadTile, q.Tile)
	}
	t1 := q.T1
	if t1 <= q.T0 {
		t1 = q.T0 + 1
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, core.ErrClosed
	}
	out := []core.Interval{}
	d := o.ds[ds]
	if d == nil {
		return out, nil
	}
	locs := d.tiles[q.Tile]
	if len(locs) == 0 {
		return out, nil
	}
	ivs, err := o.load(d, q.Tile, locs)
	if err != nil {
		return nil, err
	}
	for i := range ivs {
		iv := &ivs[i]
		if len(q.Kinds) > 0 && !hasKind(q.Kinds, iv.Kind) {
			continue
		}
		if iv.Overlaps(q.T0, t1) {
			out = append(out, *iv)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Start < b.Start
	})
	return out, nil
}

func hasKind(kinds []core.IntervalKind, k core.IntervalKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// load returns the tile's intervals (ends applied), from the cache or the log. The
// result is shared with the cache and must not be modified.
func (o *Store) load(d *dsState, tile core.TileCoord, locs []loc) ([]core.Interval, error) {
	k := cacheKey{d, tile}
	if e := o.cache[k]; e != nil {
		ce := e.Value.(*cacheEntry)
		if ce.version == len(locs) {
			o.lru.MoveToFront(e)
			o.stats.CacheHits++
			return ce.intervals, nil
		}
		o.drop(k)
	}
	o.stats.CacheMisses++
	var ivs []core.Interval
	byOrd := map[uint64]int{}
	var bytes int64
	var buf []byte
	// Read runs of adjacent records with one positioned read each.
	for i := 0; i < len(locs); {
		j, end := i+1, locs[i].off()+int64(locs[i].len())
		for j < len(locs) && locs[j].off() == end && end-locs[i].off() < 1<<20 {
			end += int64(locs[j].len())
			j++
		}
		n := int(end - locs[i].off())
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if m, err := d.f.ReadAt(buf, locs[i].off()); m < n {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("overlays: %s: %w", d.logPath(), err)
		}
		for p := 0; p < n; {
			if n-p < 8 {
				return nil, fmt.Errorf("overlays: %s: short record", d.logPath())
			}
			ln := int(binary.LittleEndian.Uint32(buf[p:]))
			if p+8+ln > n {
				return nil, fmt.Errorf("overlays: %s: short record", d.logPath())
			}
			r, err := decodeRecord(buf[p+8 : p+8+ln])
			if err != nil {
				return nil, fmt.Errorf("overlays: %s: %w", d.logPath(), err)
			}
			if r.end {
				idx, ok := byOrd[r.ord]
				if !ok {
					return nil, fmt.Errorf("overlays: %s: end of unknown interval %d", d.logPath(), r.ord)
				}
				ivs[idx].End = r.endT
			} else {
				byOrd[r.ord] = len(ivs)
				ivs = append(ivs, r.iv)
				bytes += int64(160 + len(r.iv.Key) + len(r.iv.Icon) + len(r.iv.Text) + len(r.iv.Recipe) + len(r.iv.Quality) + len(r.iv.Name))
			}
			p += 8 + ln
		}
		i = j
	}
	ce := &cacheEntry{key: k, version: len(locs), intervals: ivs, bytes: bytes}
	o.cache[k] = o.lru.PushFront(ce)
	o.cacheBytes += bytes
	for o.cacheBytes > o.opts.CacheBytes && o.lru.Len() > 1 {
		o.drop(o.lru.Back().Value.(*cacheEntry).key)
	}
	return ivs, nil
}

func (o *Store) drop(k cacheKey) {
	if e := o.cache[k]; e != nil {
		o.cacheBytes -= e.Value.(*cacheEntry).bytes
		o.lru.Remove(e)
		delete(o.cache, k)
	}
}

// Version implements core.OverlayStore. The version is the number of log records of the
// tile: every interval added or ended appends one.
func (o *Store) Version(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord) (uint64, error) {
	if tile.Z != 0 || !tile.Valid() {
		return 0, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, core.ErrClosed
	}
	if d := o.ds[ds]; d != nil {
		return uint64(len(d.tiles[tile])), nil
	}
	return 0, nil
}

// Tiles lists the z0 tiles of ds that have overlays, sorted by (y, x), with their
// versions. The static export uses it to write every overlay file.
func (o *Store) Tiles(ctx context.Context, ds core.DatastoreKey) ([]core.TileCoord, []uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, nil, core.ErrClosed
	}
	d := o.ds[ds]
	if d == nil {
		return nil, nil, nil
	}
	tiles := make([]core.TileCoord, 0, len(d.tiles))
	for t := range d.tiles {
		tiles = append(tiles, t)
	}
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].Y != tiles[j].Y {
			return tiles[i].Y < tiles[j].Y
		}
		return tiles[i].X < tiles[j].X
	})
	vers := make([]uint64, len(tiles))
	for i, t := range tiles {
		vers[i] = uint64(len(d.tiles[t]))
	}
	return tiles, vers, nil
}

// Datastores lists the datastores that have overlays, sorted by id.
func (o *Store) Datastores(ctx context.Context) ([]core.DatastoreKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, core.ErrClosed
	}
	out := make([]core.DatastoreKey, 0, len(o.ds))
	for k := range o.ds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

// Stats returns the counters and gauges.
func (o *Store) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.stats
	st.CacheBytes = o.cacheBytes
	for _, d := range o.ds {
		st.OpenIntervals += len(d.open)
		st.Tiles += len(d.tiles)
		st.LogBytes += d.size
		for _, locs := range d.tiles {
			st.IndexBytes += 64 + 8*int64(cap(locs))
		}
		for k := range d.open {
			st.IndexBytes += 100 + int64(len(k.key))
		}
	}
	return st
}

func (o *Store) closeFiles() error {
	var err error
	for _, d := range o.ds {
		if d.f != nil {
			if !o.opts.NoSync {
				d.f.Sync()
			}
			if cerr := d.f.Close(); err == nil {
				err = cerr
			}
			d.f = nil
		}
	}
	return err
}

// Close implements core.OverlayStore.
func (o *Store) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	o.cache, o.lru, o.cacheBytes = map[cacheKey]*list.Element{}, list.New(), 0
	return o.closeFiles()
}
