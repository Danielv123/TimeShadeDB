package movers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

// DefaultMinTime is the default Options.MinTime: 2016-01-01T00:00:00Z, before any
// Factorio map that can feed this store existed.
const DefaultMinTime core.TimeMs = 1_451_606_400_000

// MaxKeyframesPerCommit bounds the hourly keyframes one Commit writes for a surface. A
// surface whose first sample lies far back (a broken clock, or a long gap) gets its
// keyframes over several commits: the horizon advances at most this many hours a commit.
const MaxKeyframesPerCommit = 168

// Options tunes a Store. Zero values select the defaults.
type Options struct {
	// SyncInterval is the WAL fsync policy: 0 fsyncs every Append batch, > 0 fsyncs at
	// most that often (group commit), < 0 never fsyncs (tests and bulk imports).
	// Window, keyframe and index files are fsynced before their rename unless
	// SyncInterval < 0.
	SyncInterval time.Duration
	// NoWAL disables the write-ahead log: open-window samples are lost on a crash.
	// For bulk imports that commit at the end.
	NoWAL bool
	// LightEncoder compresses window and keyframe files at zstd's better level instead of
	// its best: about 28 MiB less memory held, files about 3% larger. For a long-running
	// server under a memory budget; imports keep the best level.
	LightEncoder bool
	// OpenBytes bounds the memory of the open windows' rows across all surfaces
	// (default 64 MiB; < 0: unbounded), counted per row (rowCost). Beyond it whole windows
	// are evicted, oldest first, and read back from their logs by the Commit that closes
	// them (open.go): Append never refuses rows for memory. Without a WAL nothing can be
	// evicted and the bound does not apply.
	OpenBytes int64
	// MaxTrailRows bounds the closed rows kept for Live (the last core.LiveTrail
	// before the horizon; default 262,144), summed over all surfaces. When exceeded
	// the oldest are dropped, so Live may miss trail rows (never last states).
	MaxTrailRows int
	// MaxLiveMovers bounds the journeys tracked as live (last row not a gone; default
	// 131,072), summed over all surfaces. Beyond it the journeys with the oldest last
	// row are forgotten, as if they had a gone: they are left out of keyframes and
	// Live.
	MaxLiveMovers int
	// MaxAhead bounds how far after the reference clock a sample may be (default 1
	// hour; < 0 disables). The reference is the later of the latest Commit upTo (at
	// Open: the newest horizon) and Clock(). Append counts later samples as Invalid
	// (Stats.Ahead), and Commit drops open windows that start beyond it, which were
	// accepted while there was no reference yet. A time in the future is impossible (a
	// broken clock); without the bound such a window would stay open forever.
	MaxAhead core.TimeMs
	// MinTime is the earliest time a sample may have (default DefaultMinTime; < 0
	// disables). Earlier samples are impossible (a broken clock) and Invalid
	// (Stats.Behind). Between it and the clock every time is accepted on a surface
	// without a horizon, however old: a backlog after a long downtime is stored.
	MinTime core.TimeMs
	// Clock, if set, returns the map clock (the server's wall clock in ms). It provides
	// the MaxAhead reference before the first Commit after Open.
	Clock func() core.TimeMs
	// Logf receives warnings about damaged files skipped at Open or in Live and about
	// WAL frames skipped at replay (default log.Printf).
	Logf func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.MaxAhead == 0 {
		o.MaxAhead = core.Hour
	}
	if o.MinTime == 0 {
		o.MinTime = DefaultMinTime
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.OpenBytes == 0 {
		o.OpenBytes = 64 << 20
	}
	if o.MaxTrailRows <= 0 {
		o.MaxTrailRows = 1 << 18
	}
	if o.MaxLiveMovers <= 0 {
		o.MaxLiveMovers = 1 << 17
	}
}

// Store is the on-disk core.MoverStore (PLAN.md §4.4, docs/v3/FORMAT.md 5.5). It keeps
// in memory only the open windows, the trail of the last core.LiveTrail and the last row
// of every live journey; closed windows, keyframes and day indexes live on disk.
type Store struct {
	opts Options
	dir  string // <db>/movers

	mu       sync.RWMutex
	closed   bool
	sfc      map[core.SurfaceKey]*surface
	openRows int   // rows in open windows, resident or not
	resident int64 // accounted memory of the open windows (rows and indexes)
	seq      uint64
	stats    Stats

	// lmu guards the window logs' files (open.go); taken after mu. dirty holds the logs
	// written since the last fsync; walErr is a sticky write error; buf is scratch.
	lmu    sync.Mutex
	dirty  map[*winLog]struct{}
	walErr error
	buf    []byte
	// upTo is the latest Commit upTo (at Open, the newest horizon); hasUpTo is false
	// until either exists.
	upTo    core.TimeMs
	hasUpTo bool
	// corrupt counts damaged window and keyframe files skipped (Live updates it without
	// the write lock); liveReadBacks counts evicted windows Live read back.
	corrupt       atomic.Int64
	liveReadBacks atomic.Int64

	stop chan struct{}
	done chan struct{}
}

var _ core.MoverStore = (*Store)(nil)

// Stats are counters and gauges for /metrics and tests.
type Stats struct {
	Accepted, Late, Invalid int64
	WindowsWritten          int64
	KeyframesWritten        int64
	BytesWritten            int64 // window and keyframe files
	OpenRows                int   // gauge: rows in open windows
	OpenBytes               int64 // gauge: accounted memory of the open windows
	SpilledWindows          int   // gauge: open windows evicted to their logs
	TrailRows               int   // gauge
	LiveMovers              int   // gauge
	Evicted                 int64 // live journeys forgotten by MaxLiveMovers
	// Evictions counts open windows evicted from memory to their logs.
	Evictions int64
	// Duplicates counts rows a Commit removed because a later arrival had the same
	// (journey, t): repeats, or rows of a forwarder that lost the time (several samples
	// of a journey stamped at one instant).
	Duplicates int64
	// Ahead counts samples beyond Options.MaxAhead, rejected by Append (Invalid) or
	// dropped with their open window by Commit; Behind counts samples before
	// Options.MinTime (Invalid).
	Ahead, Behind int64
	// Corrupt counts window and keyframe files that did not decode and were skipped
	// while rebuilding live state (Open, and Live before the horizon).
	Corrupt int64
	// WALSkipped counts WAL frames with a valid CRC that did not decode at Open.
	WALSkipped int64
	// LiveReadBacks counts evicted windows the live payload read back from their logs
	// (at most the windows of the last LiveTrail per request).
	LiveReadBacks int64
}

// surface is the per-surface state.
type surface struct {
	key core.SurfaceKey
	id  string
	dir string

	hasHorizon  bool
	horizon     core.TimeMs
	hasData     bool
	first, last core.TimeMs

	// open holds the open windows (rows with t >= horizon) by start (open.go).
	open map[core.TimeMs]*openWin
	// live is the last closed row of every journey whose last closed row is not a gone:
	// the state at the horizon.
	live map[string]*row
	// trail holds the closed rows with t > horizon - LiveTrail, sorted by t.
	trail []*row
	// rails follows each live train's rail path over the closed rows (the state at the
	// horizon), orails over the open rows after them in arrival order (made from rails' on a
	// journey's first open row). They give rows their bodies (row.body).
	rails, orails rails
	// metas interns MoverMeta values.
	metas map[core.MoverMeta]*core.MoverMeta
	// lastOpen is, per journey with open rows, the meta of its newest open row (nil when
	// that row is a gone) and that row's time (LastMeta).
	lastOpen map[string]lastMeta
	// days caches day indexes touched in this process (without Final); dayList is the
	// sorted set of days that have an index on disk.
	days    map[string]*core.DayIndex
	dayList []string
}

type surfaceMeta struct {
	Key           core.SurfaceKey `json:"key"`
	FormatVersion int             `json:"format_version"`
	Horizon       *core.TimeMs    `json:"horizon,omitempty"`
	First         *core.TimeMs    `json:"first,omitempty"`
	Last          *core.TimeMs    `json:"last,omitempty"`
}

// Open opens (or creates) the mover store under <db>/movers.
func Open(db string, opts Options) (*Store, error) {
	opts.defaults()
	m := &Store{opts: opts, dir: filepath.Join(db, "movers"), sfc: map[core.SurfaceKey]*surface{}, dirty: map[*winLog]struct{}{}}
	if _, _, err := codec(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "wal" {
			continue
		}
		key, err := core.ParseSurfaceID(e.Name())
		if err != nil {
			continue // unknown files are ignored (FORMAT.md 5)
		}
		s, err := m.loadSurface(key)
		if err != nil {
			return nil, fmt.Errorf("movers: surface %s: %w", e.Name(), err)
		}
		if s != nil {
			m.sfc[key] = s
			if s.hasHorizon && (!m.hasUpTo || s.horizon > m.upTo) {
				m.upTo, m.hasUpTo = s.horizon, true
			}
		}
	}
	m.rebalance()
	if !opts.NoWAL {
		if err := m.replayLogs(); err != nil {
			m.closeLogs()
			return nil, fmt.Errorf("movers: wal: %w", err)
		}
		if err := m.migrateWAL(); err != nil {
			m.closeLogs()
			return nil, fmt.Errorf("movers: wal: %w", err)
		}
		if opts.SyncInterval > 0 {
			m.stop, m.done = make(chan struct{}), make(chan struct{})
			go m.syncLoop()
		}
	}
	return m, nil
}

// closeLogs closes every window log (fsyncing what was written).
func (m *Store) closeLogs() error {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	var first error
	for _, s := range m.sfc {
		for _, w := range s.open {
			if err := m.closeLog(w.log); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (m *Store) syncLoop() {
	defer close(m.done)
	t := time.NewTicker(m.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			if err := m.syncLogs(); err != nil {
				m.opts.Logf("movers: wal sync: %v", err)
			}
		}
	}
}

func (m *Store) newSurface(key core.SurfaceKey) *surface {
	id := key.ID()
	return &surface{
		key:      key,
		id:       id,
		dir:      filepath.Join(m.dir, id),
		open:     map[core.TimeMs]*openWin{},
		lastOpen: map[string]lastMeta{},
		live:     map[string]*row{},
		rails:    rails{},
		orails:   rails{},
		metas:    map[core.MoverMeta]*core.MoverMeta{},
		days:     map[string]*core.DayIndex{},
	}
}

// lastMeta is a journey's newest open row: its meta (nil for a gone), time and position.
type lastMeta struct {
	meta *core.MoverMeta
	t    core.TimeMs
	x, y int32
}

// noteOpen records r, a row joining the open windows, in lastOpen.
func (s *surface) noteOpen(r *row) {
	if cur, ok := s.lastOpen[r.meta.ID]; ok && cur.t > r.t {
		return
	}
	lm := lastMeta{meta: r.meta, t: r.t, x: r.x, y: r.y}
	if r.gone() {
		lm.meta = nil
	}
	s.lastOpen[r.meta.ID] = lm
}

// LastMeta returns the meta of journey id's newest row on surface sfc when the journey
// is live there (its newest row, open or closed, is not a gone). Ingest's journey rules
// use it to complete gones of journeys they forgot (movers.Journeys.Lookup).
func (m *Store) LastMeta(sfc core.SurfaceKey, id string) (core.MoverMeta, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.sfc[sfc]
	if s == nil {
		return core.MoverMeta{}, false
	}
	if lm, ok := s.lastOpen[id]; ok {
		if lm.meta == nil {
			return core.MoverMeta{}, false
		}
		return *lm.meta, true
	}
	if r := s.live[id]; r != nil {
		return *r.meta, true
	}
	return core.MoverMeta{}, false
}

// LiveJourney is a journey live on a surface of the store (LiveJourneys): its newest row
// there, open or closed, is not a gone.
type LiveJourney struct {
	Surface core.SurfaceKey
	Meta    core.MoverMeta
	// X, Y and T are the newest row's position and time.
	X, Y float64
	T    core.TimeMs
}

// LiveJourneys returns the journeys live on the surfaces of map mapKey (as LastMeta sees
// them) for which keep, given the surface, the journey and its newest row's position,
// reports true; a journey live on two surfaces is listed once per surface. keep runs
// under the store's read lock and must not call the store. Ingest's journey rules use it
// at a resync, to end the journeys of the resyncing source they forgot (Journeys.Live).
// It walks every live journey of the map's surfaces once and copies only those kept.
func (m *Store) LiveJourneys(mapKey string, keep func(sfc core.SurfaceKey, id string, x, y float64) bool) []LiveJourney {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []LiveJourney
	add := func(s *surface, meta *core.MoverMeta, t core.TimeMs, x, y int32) {
		fx, fy := float64(x)/core.PosUnits, float64(y)/core.PosUnits
		if keep(s.key, meta.ID, fx, fy) {
			out = append(out, LiveJourney{Surface: s.key, Meta: *meta, X: fx, Y: fy, T: t})
		}
	}
	for key, s := range m.sfc {
		if key.Map != mapKey {
			continue
		}
		// A journey's newest open row is newer than its closed rows.
		for _, lm := range s.lastOpen {
			if lm.meta != nil {
				add(s, lm.meta, lm.t, lm.x, lm.y)
			}
		}
		for id, r := range s.live {
			if _, ok := s.lastOpen[id]; !ok {
				add(s, r.meta, r.t, r.x, r.y)
			}
		}
	}
	return out
}

func (s *surface) intern(meta *core.MoverMeta) *core.MoverMeta {
	if p := s.metas[*meta]; p != nil {
		return p
	}
	p := new(core.MoverMeta)
	*p = *meta
	s.metas[*p] = p
	return p
}

func (s *surface) observe(t core.TimeMs) {
	if !s.hasData || t < s.first {
		s.first = t
	}
	if !s.hasData || t > s.last {
		s.last = t
	}
	s.hasData = true
}

// ref returns the reference clock of Options.MaxAhead and MaxBehind.
func (m *Store) ref() (core.TimeMs, bool) {
	t, ok := m.upTo, m.hasUpTo
	if m.opts.Clock != nil {
		if c := m.opts.Clock(); !ok || c > t {
			t, ok = c, true
		}
	}
	return t, ok
}

// Pressure returns the share of Options.OpenBytes the open windows hold in memory. A
// server commits early when it grows; beyond 1 whole windows are evicted to their logs.
func (m *Store) Pressure() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.opts.OpenBytes <= 0 {
		return 0
	}
	return float64(m.resident) / float64(m.opts.OpenBytes)
}

// Append implements core.MoverStore. The batch is checked and logged to the window logs
// before anything changes in memory: when a log write fails nothing is applied (the logs
// written are cut back) and the result is zero. Append never refuses rows for memory:
// beyond Options.OpenBytes it evicts whole open windows to their logs.
func (m *Store) Append(ctx context.Context, samples []core.MoverSample) (core.ApplyResult, error) {
	var res core.ApplyResult
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return res, core.ErrClosed
	}
	if !m.opts.NoWAL {
		m.lmu.Lock()
		err := m.walErr
		m.lmu.Unlock()
		if err != nil {
			return res, fmt.Errorf("movers: wal: %w", err)
		}
	}
	ref, hasRef := m.ref()
	var ahead, behind int64
	type pending struct {
		s   *surface
		key core.SurfaceKey
		r   row
		w   *openWin
	}
	accepted := make([]pending, 0, len(samples))
	for i := range samples {
		smp := &samples[i]
		if smp.Surface.Validate() != nil || smp.Meta.ID == "" || !smp.Meta.Kind.Valid() {
			res.Invalid++
			continue
		}
		s := m.sfc[smp.Surface]
		if s != nil && s.hasHorizon && smp.T < s.horizon {
			res.Late++
			continue
		}
		if hasRef && m.opts.MaxAhead >= 0 && smp.T > ref+m.opts.MaxAhead {
			res.Invalid++
			ahead++
			continue
		}
		if m.opts.MinTime >= 0 && smp.T < m.opts.MinTime {
			res.Invalid++
			behind++
			continue
		}
		r, ok := makeRow(smp, &smp.Meta, 0)
		if !ok {
			res.Invalid++
			continue
		}
		accepted = append(accepted, pending{s: s, key: smp.Surface, r: r})
	}
	// The windows (and surfaces) the rows go to; those made here are removed again when
	// the logs cannot be written.
	var newSurfaces []*surface
	var newWins []*openWin
	var batches []logBatch
	byWin := map[*openWin]int{}
	undo := func() {
		for _, w := range newWins {
			for _, s := range m.sfc {
				if s.open[w.start] == w && w.n == 0 {
					delete(s.open, w.start)
				}
			}
		}
		for _, s := range newSurfaces {
			if len(s.open) == 0 && !s.hasData {
				delete(m.sfc, s.key)
			}
		}
	}
	for i := range accepted {
		p := &accepted[i]
		if p.s == nil {
			if p.s = m.sfc[p.key]; p.s == nil {
				p.s = m.newSurface(p.key)
				m.sfc[p.key] = p.s
				newSurfaces = append(newSurfaces, p.s)
			}
		}
		ws := core.WindowStart(p.r.t, core.MoverWindowLen)
		w := p.s.open[ws]
		if w == nil {
			var err error
			if w, err = m.win(p.s, ws); err != nil {
				undo()
				return core.ApplyResult{}, fmt.Errorf("movers: wal: %w", err)
			}
			newWins = append(newWins, w)
		}
		p.w = w
		if w.log != nil {
			k, ok := byWin[w]
			if !ok {
				k = len(batches)
				byWin[w] = k
				batches = append(batches, logBatch{log: w.log})
			}
			batches[k].entries = append(batches[k].entries, walEntry{sfc: p.key, r: &p.r})
		}
	}
	if len(batches) > 0 {
		if err := m.writeLogs(batches); err != nil {
			undo()
			return core.ApplyResult{}, fmt.Errorf("movers: wal: %w", err)
		}
	}
	redo := map[*surface]map[string]bool{}
	for i := range accepted {
		p := &accepted[i]
		m.seq++
		p.r.seq = m.seq
		p.r.meta = p.s.intern(p.r.meta)
		p.s.openBody(&p.r, redo)
		p.s.observe(p.r.t)
		p.s.noteOpen(&p.r)
		m.addRow(p.w, p.r)
	}
	for s, ids := range redo {
		if len(ids) > 0 {
			m.rebody(s, ids)
		}
	}
	m.evict()
	res.Accepted = len(accepted)
	m.stats.Accepted += int64(res.Accepted)
	m.stats.Late += int64(res.Late)
	m.stats.Invalid += int64(res.Invalid)
	m.stats.Ahead += ahead
	m.stats.Behind += behind
	return res, nil
}

// openBody gives r, a row about to join an open window, its body from its journey's RailBody over
// the open rows (orails). A row at or before the journey's previous open row arrived out of
// time order: its journey is noted in redo for rebody, and its body waits for that. A
// repeat of the previous row (a retried sample) needs no redo.
func (s *surface) openBody(r *row, redo map[*surface]map[string]bool) {
	r.body = nil
	if r.meta.Kind != core.KindTrain || redo[s][r.meta.ID] {
		return
	}
	b := s.orails.get(r.meta.ID, s.rails)
	rr := r.railRow()
	if t, ok := b.Last(); ok && r.t <= t && !b.Repeats(rr) {
		if redo[s] == nil {
			redo[s] = map[string]bool{}
		}
		redo[s][r.meta.ID] = true
		return
	}
	r.body = b.Add(rr)
}

// rebody gives the open rows of the journeys in ids their bodies again, from the state at
// the horizon and their open rows in time order (for equal times the last arrival, which
// Commit keeps; the rows it replaces get the same body). An evicted window that holds rows
// of one of them is read back from its log, one window at a time; its index takes the new
// bodies.
func (m *Store) rebody(s *surface, ids map[string]bool) {
	for id := range ids {
		delete(s.orails, id)
	}
	m.follow(s, ids)
}

// follow follows the open rows of the journeys in ids (every journey when ids is nil),
// window by window in time order, with their RailBodies in s.orails (begun from s.rails
// when missing), and gives them their bodies. Rows of one journey in an earlier window are
// all earlier in time, so window by window is time order. An evicted window is read back
// from its log only when it holds one of the journeys.
func (m *Store) follow(s *surface, ids map[string]bool) {
	starts := make([]core.TimeMs, 0, len(s.open))
	for ws := range s.open {
		starts = append(starts, ws)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, ws := range starts {
		w := s.open[ws]
		rows := w.rows
		if w.spilled {
			need := ids == nil
			for id := range ids {
				if _, ok := w.last[id]; ok {
					need = true
					break
				}
			}
			if !need {
				continue
			}
			var err error
			if rows, err = m.readWinRows(s, w); err != nil {
				m.opts.Logf("movers: train bodies of %s: %v", s.id, err)
				continue
			}
		}
		ptrs := make([]*row, 0, len(rows))
		for i := range rows {
			if ids == nil || ids[rows[i].meta.ID] {
				ptrs = append(ptrs, &rows[i])
			}
		}
		sortByJourney(ptrs)
		followRows(s.orails, s.rails, ptrs)
		if w.spilled {
			for _, r := range ptrs {
				if lr := w.last[r.meta.ID]; lr != nil && lr.t == r.t {
					lr.body = r.body
				}
			}
		}
	}
}

// Commit implements core.MoverStore.
func (m *Store) Commit(ctx context.Context, upTo core.TimeMs) error {
	m.mu.Lock()
	if !m.closed && (!m.hasUpTo || upTo > m.upTo) {
		m.upTo, m.hasUpTo = upTo, true
	}
	m.mu.Unlock()
	return m.CommitBy(ctx, func(core.SurfaceKey) core.TimeMs { return upTo })
}

// CommitBy is Commit with a target per surface: it closes the windows of each surface
// that end at or before upTo(surface) and raises its horizon to the start of the window
// holding it. A server that holds the horizon of a map back while one of its sources is
// still sending older data (a backlog after a downtime) passes a lower target for the
// surfaces of that map; their windows stay open (evicted to their logs when memory runs
// short) until the source has caught up.
func (m *Store) CommitBy(ctx context.Context, upTo func(core.SurfaceKey) core.TimeMs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return core.ErrClosed
	}
	keys := make([]core.SurfaceKey, 0, len(m.sfc))
	for k := range m.sfc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID() < keys[j].ID() })
	targets := make([]core.TimeMs, len(keys))
	for i, k := range keys {
		targets[i] = upTo(k)
		if !m.hasUpTo || targets[i] > m.upTo {
			m.upTo, m.hasUpTo = targets[i], true
		}
	}
	m.dropOutOfRange(keys)
	for i, k := range keys {
		s := m.sfc[k]
		newH := core.WindowStart(targets[i], core.MoverWindowLen)
		if s.hasHorizon && newH <= s.horizon {
			continue
		}
		if err := m.commitSurface(s, newH); err != nil {
			return fmt.Errorf("movers: commit %s: %w", s.id, err)
		}
	}
	// The surfaces committed first above could take what later ones then claimed.
	m.rebalance()
	return nil
}

// dropOutOfRange drops the open windows that start more than Options.MaxAhead after the
// reference clock. Append rejects such samples once a reference exists, so this only
// finds rows accepted before the first Commit (or Clock) of a fresh store.
func (m *Store) dropOutOfRange(keys []core.SurfaceKey) {
	ref, ok := m.ref()
	if !ok || m.opts.MaxAhead < 0 {
		return
	}
	for _, k := range keys {
		s := m.sfc[k]
		dropped := false
		for ws, w := range s.open {
			if ws > ref+m.opts.MaxAhead {
				m.stats.Ahead += int64(w.n)
				m.dropWin(s, w)
				dropped = true
			}
		}
		if dropped && !s.hasHorizon {
			// first and last came from the open rows alone.
			s.hasData = false
			for _, w := range s.open {
				if w.n > 0 {
					s.observe(w.tmin)
					s.observe(w.tmax)
				}
			}
		}
	}
}

// dropWin removes an open window and its log. Callers hold m.mu for writing.
func (m *Store) dropWin(s *surface, w *openWin) {
	m.openRows -= w.n
	m.resident -= w.bytes + w.ibytes
	delete(s.open, w.start)
	m.lmu.Lock()
	err := m.removeLog(w.log)
	m.lmu.Unlock()
	if err != nil {
		// Open deletes it once the horizon is past it; until then it would be replayed.
		m.opts.Logf("movers: removing %s: %v", w.log.path, err)
	}
}

// commitSurface closes the windows of s that end at or before newH, one window at a time
// (an evicted window is read back from its log). The new live state and trail are built on
// copies and installed, with the new horizon, only once meta.json is written: after a
// failure the surface is unchanged and the Commit can be retried (files already written
// are written again with the same or newer content). The closed windows' logs are deleted
// after that; Open deletes any a crash left behind.
func (m *Store) commitSurface(s *surface, newH core.TimeMs) error {
	sync := m.opts.SyncInterval >= 0
	// Keyframes at every UTC hour h with oldH < h <= newH and first < h, at most
	// MaxKeyframesPerCommit of them: the horizon catches up over the next commits.
	var h core.TimeMs = math.MaxInt64
	if s.hasData {
		h = core.WindowStart(s.first, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
		if s.hasHorizon && h <= s.horizon {
			h = core.WindowStart(s.horizon, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
		}
		if last := h + (MaxKeyframesPerCommit-1)*core.MoverKeyframeEvery; newH > last {
			newH = last
		}
	}
	if s.hasHorizon && newH <= s.horizon {
		return nil
	}
	starts := make([]core.TimeMs, 0, len(s.open))
	for ws := range s.open {
		if ws < newH {
			starts = append(starts, ws)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	live := maps.Clone(s.live)
	cr := s.rails.clone()
	reseeded := false
	touched := map[string]bool{}
	emitKeyframes := func(upTo core.TimeMs) error {
		for ; h <= newH && h <= upTo; h += core.MoverKeyframeEvery {
			if err := m.writeKeyframe(s, h, live, sync); err != nil {
				return err
			}
			cr = reseed(live)
			reseeded = true
			touched[core.UTCDay(h)] = true
		}
		return nil
	}
	cut := newH - core.LiveTrail
	var closedTrail []*row
	var dups int64
	for _, ws := range starts {
		if err := emitKeyframes(ws); err != nil {
			return err
		}
		w := s.open[ws]
		rows, err := m.windowRows(s, w)
		if err != nil {
			return err
		}
		// Dedupe: for equal (journey, t) the last arrival wins.
		ptrs := make([]*row, len(rows))
		for i := range rows {
			ptrs[i] = &rows[i]
		}
		sortByJourney(ptrs)
		n0 := len(ptrs)
		ptrs = dedupe(ptrs)
		dups += int64(n0 - len(ptrs))
		markHandoffs(s, ptrs, ws)
		if len(ptrs) > 0 {
			if err := m.writeWindow(s, ws, ptrs, sync); err != nil {
				return err
			}
			touched[core.UTCDay(ws)] = true
		}
		// ptrs holds each journey's rows in time order (sorted by journey and t)
		for _, r := range ptrs {
			cr.add(r, nil)
		}
		advance(live, ptrs)
		for _, r := range ptrs {
			if r.t > cut {
				c := *r
				closedTrail = append(closedTrail, &c)
			}
		}
	}
	if err := emitKeyframes(newH); err != nil {
		return err
	}
	// Trail: closed rows with t > newH - LiveTrail.
	trail := make([]*row, 0, len(s.trail)+len(closedTrail))
	for _, r := range s.trail {
		if r.t > cut {
			trail = append(trail, r)
		}
	}
	trail = append(trail, closedTrail...)
	sort.SliceStable(trail, func(i, j int) bool { return trail[i].t < trail[j].t })
	if lim := m.share(s, m.opts.MaxTrailRows, trailLen); len(trail) > lim {
		trail = append([]*row(nil), trail[len(trail)-lim:]...)
	}
	evicted := evictLive(live, m.share(s, m.opts.MaxLiveMovers, liveLen))
	for id := range cr {
		if live[id] == nil {
			delete(cr, id)
		}
	}
	// Day indexes, then the surface meta (the horizon) last.
	for d := range touched {
		if err := s.writeDay(d, sync); err != nil {
			return err
		}
	}
	if err := s.writeMeta(true, newH, sync); err != nil {
		return err
	}
	s.hasHorizon, s.horizon = true, newH
	s.live, s.trail = live, trail
	s.rails = cr
	m.stats.Evicted += evicted
	m.stats.Duplicates += dups
	for _, ws := range starts {
		m.dropWin(s, s.open[ws])
	}
	s.lastOpen = map[string]lastMeta{}
	for _, w := range s.open {
		for i := range w.rows {
			s.noteOpen(&w.rows[i])
		}
		for _, r := range w.last {
			s.noteOpen(r)
		}
	}
	// The rows that stay open get their bodies again, from the new state at the horizon.
	// A journey with rows in an evicted window keeps its RailBody, which followed those
	// rows already (in arrival order), unless this commit wrote a keyframe, which begins
	// every RailBody again at its row: then the evicted windows are read back.
	keep := rails{}
	ids := map[string]bool{}
	for _, w := range s.open {
		if w.spilled && !reseeded {
			for id := range w.last {
				if b := s.orails[id]; b != nil {
					keep[id] = b
				}
			}
		}
	}
	for _, w := range s.open {
		for i := range w.rows {
			if id := w.rows[i].meta.ID; keep[id] == nil {
				ids[id] = true
			}
		}
		if w.spilled && reseeded {
			for id := range w.last {
				ids[id] = true
			}
		}
	}
	s.orails = keep
	m.follow(s, ids)
	// Drop cached day indexes that can no longer change.
	today := core.UTCDay(newH)
	for d := range s.days {
		if d < today {
			delete(s.days, d)
		}
	}
	s.reintern()
	return nil
}

// followRows gives rows, sorted by journey, t and arrival, their bodies from their
// journeys' RailBody in rs (made from base's when missing); rows with equal (journey, t)
// get the body of the last arrival, which Commit keeps.
func followRows(rs, base rails, rows []*row) {
	for i := 0; i < len(rows); {
		j := i
		for j+1 < len(rows) && rows[j+1].meta.ID == rows[i].meta.ID && rows[j+1].t == rows[i].t {
			j++
		}
		var body []core.RailPiece
		if r := rows[j]; r.meta.Kind == core.KindTrain {
			body = rs.get(r.meta.ID, base).Add(r.railRow())
		}
		for k := i; k <= j; k++ {
			rows[k].body = body
		}
		i = j + 1
	}
}

// markHandoffs flags the gones of window ws (rows sorted by journey and t, deduplicated)
// whose journey has a sample in (T, T+HandoffWindow] among all rows known now: later in
// the window, or early in the next open window.
func markHandoffs(s *surface, rows []*row, ws core.TimeMs) {
	var next map[string]core.TimeMs
	nextDone := false
	for i, r := range rows {
		if !r.gone() {
			continue
		}
		found := false
		for j := i + 1; j < len(rows) && rows[j].meta.ID == r.meta.ID && rows[j].t <= r.t+core.HandoffWindow; j++ {
			if !rows[j].gone() && rows[j].t > r.t {
				found = true
				break
			}
		}
		if !found && r.t+core.HandoffWindow >= ws+core.MoverWindowLen {
			if !nextDone {
				next, nextDone = s.firstSamples(ws+core.MoverWindowLen), true
			}
			if t, ok := next[r.meta.ID]; ok && t <= r.t+core.HandoffWindow {
				found = true
			}
		}
		if found {
			r.flags |= core.FlagHandoff
		}
	}
}

// firstSamples returns, per journey, the earliest t of a sample that is not a gone in the
// open window starting at ws (nil when there is none).
func (s *surface) firstSamples(ws core.TimeMs) map[string]core.TimeMs {
	w := s.open[ws]
	if w == nil {
		return nil
	}
	if w.spilled {
		return w.first
	}
	out := map[string]core.TimeMs{}
	for i := range w.rows {
		r := &w.rows[i]
		if r.gone() {
			continue
		}
		if t, ok := out[r.meta.ID]; !ok || r.t < t {
			out[r.meta.ID] = r.t
		}
	}
	return out
}

// writeWindow writes closed window ws of s (rows sorted by journey and t, without equal
// (journey, t)) and records it in its day index.
func (m *Store) writeWindow(s *surface, ws core.TimeMs, rows []*row, sync bool) error {
	w := buildWindow(s.key, core.WindowClosed, ws, core.MoverWindowLen, rows)
	body, err := wire.EncodeMoverWindow(w)
	if err != nil {
		return err
	}
	file, err := encodeWindowFile(body, m.opts.LightEncoder)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.windowPath(ws), file, sync); err != nil {
		return err
	}
	m.stats.WindowsWritten++
	m.stats.BytesWritten += int64(len(file))
	ref := core.WindowRef{Start: ws, First: rows[0].t, Last: rows[0].t, Rows: len(rows)}
	for _, r := range rows {
		ref.First = min(ref.First, r.t)
		ref.Last = max(ref.Last, r.t)
	}
	day, err := s.day(core.UTCDay(ws))
	if err != nil {
		return err
	}
	i := sort.Search(len(day.Windows), func(i int) bool { return day.Windows[i].Start >= ws })
	if i < len(day.Windows) && day.Windows[i].Start == ws {
		day.Windows[i] = ref
	} else {
		day.Windows = append(day.Windows, core.WindowRef{})
		copy(day.Windows[i+1:], day.Windows[i:])
		day.Windows[i] = ref
	}
	return nil
}

// advance applies closed rows to a live map. Rows of one journey must come in t order,
// and after every row applied earlier.
func advance(live map[string]*row, rows []*row) {
	for _, r := range rows {
		if r.gone() {
			delete(live, r.meta.ID)
			continue
		}
		c := *r
		live[r.meta.ID] = &c
	}
}

func trailLen(s *surface) int { return len(s.trail) }
func liveLen(s *surface) int  { return len(s.live) }

// share returns how much of a limit shared by all surfaces (MaxTrailRows,
// MaxLiveMovers) surface s may use: what the other surfaces leave of it, but at least
// an equal share, so a busy surface cannot starve the others for good. Each surface is
// trimmed when it commits, so a surface that grew before others appeared gives back
// its excess at its next commit. Callers hold m.mu for writing.
func (m *Store) share(s *surface, limit int, used func(*surface) int) int {
	others, n := 0, len(m.sfc)
	if m.sfc[s.key] != s {
		n++ // s is being loaded and not in the map yet
	}
	for _, o := range m.sfc {
		if o != s {
			others += used(o)
		}
	}
	return max(limit/max(n, 1), limit-others, 1)
}

// rebalance trims every surface's trail and live map to its share, so the totals stay
// within the limits. Open loads each surface with the whole limits and calls it once
// all are loaded; Commit calls it after committing every surface. Callers hold m.mu
// for writing.
func (m *Store) rebalance() {
	keys := make([]core.SurfaceKey, 0, len(m.sfc))
	for k := range m.sfc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID() < keys[j].ID() })
	for _, k := range keys {
		s := m.sfc[k]
		if lim := m.share(s, m.opts.MaxTrailRows, trailLen); len(s.trail) > lim {
			s.trail = append([]*row(nil), s.trail[len(s.trail)-lim:]...)
		}
		m.stats.Evicted += evictLive(s.live, m.share(s, m.opts.MaxLiveMovers, liveLen))
	}
}

// evictLive keeps at most limit journeys of a live map, forgetting those with the
// oldest last row, and returns how many it forgot.
func evictLive(live map[string]*row, limit int) int64 {
	if len(live) <= limit {
		return 0
	}
	all := make([]*row, 0, len(live))
	for _, r := range live {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].t != all[j].t {
			return all[i].t < all[j].t
		}
		return all[i].meta.ID < all[j].meta.ID
	})
	n := len(all) - limit
	for _, r := range all[:n] {
		delete(live, r.meta.ID)
	}
	return int64(n)
}

// reintern rebuilds the meta table from the rows still referenced, so it stays bounded.
func (s *surface) reintern() {
	metas := make(map[core.MoverMeta]*core.MoverMeta, len(s.live))
	keep := func(p *core.MoverMeta) {
		if _, ok := metas[*p]; !ok {
			metas[*p] = p
		}
	}
	for _, r := range s.live {
		keep(r.meta)
	}
	for _, w := range s.open {
		for i := range w.rows {
			keep(w.rows[i].meta)
		}
		for _, r := range w.last {
			keep(r.meta)
		}
	}
	for _, lm := range s.lastOpen {
		if lm.meta != nil {
			keep(lm.meta)
		}
	}
	for _, r := range s.trail {
		keep(r.meta)
	}
	s.metas = metas
}

func (m *Store) writeKeyframe(s *surface, h core.TimeMs, live map[string]*row, sync bool) error {
	rows := make([]*row, 0, len(live))
	for _, r := range live {
		rows = append(rows, r.withBody())
	}
	w := buildWindow(s.key, core.WindowKeyframe, h, 0, rows)
	body, err := wire.EncodeMoverWindow(w)
	if err != nil {
		return err
	}
	file, err := encodeWindowFile(body, m.opts.LightEncoder)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.keyframePath(h), file, sync); err != nil {
		return err
	}
	m.stats.KeyframesWritten++
	m.stats.BytesWritten += int64(len(file))
	day, err := s.day(core.UTCDay(h))
	if err != nil {
		return err
	}
	i := sort.Search(len(day.Keyframes), func(i int) bool { return day.Keyframes[i] >= h })
	if i == len(day.Keyframes) || day.Keyframes[i] != h {
		day.Keyframes = append(day.Keyframes, 0)
		copy(day.Keyframes[i+1:], day.Keyframes[i:])
		day.Keyframes[i] = h
	}
	return nil
}

// day returns the cached day index for date, loading it from disk or creating it.
func (s *surface) day(date string) (*core.DayIndex, error) {
	if d := s.days[date]; d != nil {
		return d, nil
	}
	d, err := s.readDay(date)
	if errors.Is(err, os.ErrNotExist) {
		d = &core.DayIndex{Date: date, Windows: []core.WindowRef{}, Keyframes: []core.TimeMs{}}
	} else if err != nil {
		return nil, err
	}
	s.days[date] = d
	return d, nil
}

func (s *surface) readDay(date string) (*core.DayIndex, error) {
	b, err := os.ReadFile(s.dayPath(date))
	if err != nil {
		return nil, err
	}
	d := &core.DayIndex{}
	if err := json.Unmarshal(b, d); err != nil {
		return nil, fmt.Errorf("day index %s: %w", date, err)
	}
	if d.Windows == nil {
		d.Windows = []core.WindowRef{}
	}
	if d.Keyframes == nil {
		d.Keyframes = []core.TimeMs{}
	}
	d.Final = false
	return d, nil
}

func (s *surface) writeDay(date string, sync bool) error {
	d := s.days[date]
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dayPath(date), b, sync); err != nil {
		return err
	}
	i := sort.SearchStrings(s.dayList, date)
	if i == len(s.dayList) || s.dayList[i] != date {
		s.dayList = append(s.dayList, "")
		copy(s.dayList[i+1:], s.dayList[i:])
		s.dayList[i] = date
	}
	return nil
}

// writeMeta writes meta.json with the given horizon; the caller sets the surface's
// fields once it succeeded.
func (s *surface) writeMeta(hasHorizon bool, horizon core.TimeMs, sync bool) error {
	sm := surfaceMeta{Key: s.key, FormatVersion: FormatVersion}
	if hasHorizon {
		h := horizon
		sm.Horizon = &h
	}
	if s.hasData {
		f, l := s.first, s.last
		sm.First, sm.Last = &f, &l
	}
	b, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "meta.json"), b, sync)
}

// loadSurface restores a surface from disk: meta, day list, and the live state at the
// horizon from the newest keyframe plus the windows after it. It returns nil for a
// directory without meta.json.
func (m *Store) loadSurface(key core.SurfaceKey) (*surface, error) {
	s := m.newSurface(key)
	b, err := os.ReadFile(filepath.Join(s.dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sm surfaceMeta
	if err := json.Unmarshal(b, &sm); err != nil {
		return nil, fmt.Errorf("meta.json: %w", err)
	}
	if sm.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("format_version %d is newer than %d", sm.FormatVersion, FormatVersion)
	}
	if sm.Key != key {
		return nil, fmt.Errorf("meta.json key %v does not match the directory", sm.Key)
	}
	if sm.Horizon != nil {
		s.hasHorizon, s.horizon = true, *sm.Horizon
	}
	if sm.First != nil && sm.Last != nil {
		s.hasData, s.first, s.last = true, *sm.First, *sm.Last
	}
	ents, _ := os.ReadDir(filepath.Join(s.dir, "days"))
	for _, e := range ents {
		name := e.Name()
		if d, ok := strings.CutSuffix(name, ".json"); ok && validDay(d) {
			s.dayList = append(s.dayList, d)
		}
	}
	sort.Strings(s.dayList)
	if !s.hasHorizon || !s.hasData {
		return s, nil
	}
	// The newest keyframe at or before the horizon, then every window after it.
	from := core.WindowStart(s.first, core.MoverWindowLen)
	for h := core.WindowStart(s.horizon, core.MoverKeyframeEvery); h > s.first; h -= core.MoverKeyframeEvery {
		w, err := s.readWindow(s.keyframePath(h))
		if errors.Is(err, core.ErrNoVersion) {
			continue
		}
		if errors.Is(err, errUndecodable) {
			m.damaged(s.keyframePath(h), err)
			continue // an older keyframe and more windows rebuild the state
		}
		if err != nil {
			return nil, err
		}
		for i := range w.Rows {
			r := rowFromCore(s.intern(&w.Movers[w.Rows[i].Mover]), &w.Rows[i])
			if !r.gone() {
				rr := r
				s.live[r.meta.ID] = &rr
			}
		}
		s.rails = seedRails(w)
		from = h
		break
	}
	// Windows before the keyframe are read only for the trail. Their rows take their bodies
	// from the keyframe before them, as the store that wrote them gave them (only when the
	// trail reaches back past the keyframe, just after one was written).
	cut := s.horizon - core.LiveTrail
	start := min(from, core.WindowStart(cut+1, core.MoverWindowLen))
	pre := rails{}
	if start < from {
		pk := core.WindowStart(start, core.MoverKeyframeEvery)
		if w, err := s.readWindow(s.keyframePath(pk)); err == nil {
			pre = seedRails(w)
		}
		start = max(pk, core.WindowStart(s.first, core.MoverWindowLen))
	}
	for ws := start; ws < s.horizon; ws += core.MoverWindowLen {
		if !s.hasWindow(ws) {
			continue
		}
		w, err := s.readWindow(s.windowPath(ws))
		if errors.Is(err, core.ErrNoVersion) {
			continue
		}
		if errors.Is(err, errUndecodable) {
			m.damaged(s.windowPath(ws), err)
			continue
		}
		if err != nil {
			return nil, err
		}
		rows := make([]*row, len(w.Rows))
		for i := range w.Rows {
			r := rowFromCore(s.intern(&w.Movers[w.Rows[i].Mover]), &w.Rows[i])
			rows[i] = &r
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
		rs := pre
		if ws >= from {
			rs = s.rails
		}
		for _, r := range rows {
			rs.add(r, nil)
		}
		if ws >= from {
			advance(s.live, rows)
		}
		for _, r := range rows {
			if r.t > cut {
				c := *r
				s.trail = append(s.trail, &c)
			}
		}
	}
	sort.SliceStable(s.trail, func(i, j int) bool { return s.trail[i].t < s.trail[j].t })
	if len(s.trail) > m.opts.MaxTrailRows {
		s.trail = s.trail[len(s.trail)-m.opts.MaxTrailRows:]
	}
	m.stats.Evicted += evictLive(s.live, m.opts.MaxLiveMovers)
	for id := range s.rails {
		if s.live[id] == nil {
			delete(s.rails, id)
		}
	}
	return s, nil
}

// errUndecodable marks a window or keyframe file that exists but does not decode.
var errUndecodable = errors.New("undecodable mover window file")

// damaged logs and counts a window or keyframe file skipped because it does not decode.
func (m *Store) damaged(path string, err error) {
	m.corrupt.Add(1)
	m.opts.Logf("movers: skipping damaged file %s: %v", path, err)
}

// hasWindow reports whether the day index lists window ws (reading it if needed). When
// the index cannot be read it answers true, so the caller tries the file.
func (s *surface) hasWindow(ws core.TimeMs) bool {
	date := core.UTCDay(ws)
	i := sort.SearchStrings(s.dayList, date)
	if i == len(s.dayList) || s.dayList[i] != date {
		return false
	}
	d, err := s.day(date)
	if err != nil {
		return true
	}
	j := sort.Search(len(d.Windows), func(j int) bool { return d.Windows[j].Start >= ws })
	return j < len(d.Windows) && d.Windows[j].Start == ws
}

func validDay(d string) bool {
	t, err := core.ParseUTCDay(d)
	return err == nil && core.UTCDay(t) == d
}

// readWindow reads and decodes a window or keyframe file. A missing file is
// core.ErrNoVersion; a file that does not decode wraps errUndecodable.
func (s *surface) readWindow(path string) (*core.MoverWindow, error) {
	b, err := readWindowFile(path)
	if err != nil {
		return nil, err
	}
	return decodeFile(b, s.key)
}

func (m *Store) surface(sfc core.SurfaceKey) (*surface, error) {
	if m.closed {
		return nil, core.ErrClosed
	}
	s := m.sfc[sfc]
	if s == nil {
		return nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, sfc)
	}
	return s, nil
}

// Surfaces implements core.MoverStore.
func (m *Store) Surfaces(ctx context.Context) ([]core.SurfaceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, core.ErrClosed
	}
	out := make([]core.SurfaceInfo, 0, len(m.sfc))
	for _, s := range m.sfc {
		info := core.SurfaceInfo{Key: s.key, ID: s.id, Days: append([]string{}, s.dayList...)}
		if s.hasHorizon {
			info.Horizon = s.horizon
		}
		if s.hasData {
			info.FirstT, info.LastT = s.first, s.last
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Day implements core.MoverStore.
func (m *Store) Day(ctx context.Context, sfc core.SurfaceKey, date string) (core.DayIndex, error) {
	// Day may load an index into the cache, so it takes the write lock; it is cheap.
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.surface(sfc)
	if err != nil {
		return core.DayIndex{}, err
	}
	if !validDay(date) {
		return core.DayIndex{}, fmt.Errorf("%w: bad UTC day %q", core.ErrNoVersion, date)
	}
	i := sort.SearchStrings(s.dayList, date)
	if i == len(s.dayList) || s.dayList[i] != date {
		return core.DayIndex{}, fmt.Errorf("%w: %v has nothing on %s", core.ErrNoVersion, sfc, date)
	}
	d := s.days[date]
	if d == nil {
		if d, err = s.readDay(date); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return core.DayIndex{}, fmt.Errorf("%w: %v has nothing on %s", core.ErrNoVersion, sfc, date)
			}
			return core.DayIndex{}, err
		}
	}
	out := core.DayIndex{Date: date, Windows: append([]core.WindowRef{}, d.Windows...), Keyframes: append([]core.TimeMs{}, d.Keyframes...)}
	if len(out.Windows) == 0 && len(out.Keyframes) == 0 {
		return core.DayIndex{}, fmt.Errorf("%w: %v has nothing on %s", core.ErrNoVersion, sfc, date)
	}
	start, _ := core.ParseUTCDay(date)
	out.Final = s.hasHorizon && s.horizon >= start+core.Day
	return out, nil
}

// WindowFile returns the stored .mvw file of the closed window at start: one zstd frame
// of the MVW3 body, which the HTTP API can send unchanged with Content-Encoding: zstd.
func (m *Store) WindowFile(ctx context.Context, sfc core.SurfaceKey, start core.TimeMs) ([]byte, error) {
	m.mu.RLock()
	s, err := m.surface(sfc)
	var ok bool
	if err == nil {
		ok = s.hasHorizon && start%core.MoverWindowLen == 0 && start+core.MoverWindowLen <= s.horizon
	}
	m.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %v window %d is not closed", core.ErrNoVersion, sfc, start)
	}
	return readWindowFile(s.windowPath(start))
}

// KeyframeFile returns the stored .mvw file of the keyframe at t (see WindowFile).
func (m *Store) KeyframeFile(ctx context.Context, sfc core.SurfaceKey, t core.TimeMs) ([]byte, error) {
	m.mu.RLock()
	s, err := m.surface(sfc)
	var ok bool
	if err == nil {
		ok = s.hasHorizon && t%core.MoverKeyframeEvery == 0 && t <= s.horizon
	}
	m.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %v keyframe %d", core.ErrNoVersion, sfc, t)
	}
	return readWindowFile(s.keyframePath(t))
}

func decodeFile(b []byte, sfc core.SurfaceKey) (*core.MoverWindow, error) {
	body, err := decodeWindowFile(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUndecodable, err)
	}
	w, err := wire.DecodeMoverWindow(body, sfc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUndecodable, err)
	}
	return w, nil
}

// Window implements core.MoverStore.
func (m *Store) Window(ctx context.Context, sfc core.SurfaceKey, start core.TimeMs) (*core.MoverWindow, error) {
	b, err := m.WindowFile(ctx, sfc, start)
	if err != nil {
		return nil, err
	}
	return decodeFile(b, sfc)
}

// Keyframe implements core.MoverStore.
func (m *Store) Keyframe(ctx context.Context, sfc core.SurfaceKey, t core.TimeMs) (*core.MoverWindow, error) {
	b, err := m.KeyframeFile(ctx, sfc, t)
	if err != nil {
		return nil, err
	}
	return decodeFile(b, sfc)
}

// Live implements core.MoverStore. For now at or after the horizon it reads memory
// only; for an earlier instant it rebuilds the state from the keyframe and windows on
// disk.
func (m *Store) Live(ctx context.Context, sfc core.SurfaceKey, now core.TimeMs) (*core.MoverWindow, error) {
	m.mu.RLock()
	s, err := m.surface(sfc)
	if err != nil {
		m.mu.RUnlock()
		return nil, err
	}
	if !s.hasHorizon || now >= s.horizon {
		defer m.mu.RUnlock()
		return m.liveFromMemory(s, now)
	}
	first, hasData := s.first, s.hasData
	m.mu.RUnlock()
	return s.liveFromDisk(m, now, first, hasData)
}

// liveFromMemory builds the live payload at now >= horizon from the trail and the open
// windows. An evicted window gives each journey's newest row from its index; when one
// holds rows the payload needs beyond that (rows of the last LiveTrail, or rows after
// now), the rows of the windows the trail overlaps are read back from their logs and
// their bodies followed again (liveExact). Eviction keeps a surface's newest windows resident, so that only happens
// for an instant in the past or when the newest windows alone exceed the budget.
func (m *Store) liveFromMemory(s *surface, now core.TimeMs) (*core.MoverWindow, error) {
	cut := now - core.LiveTrail
	for _, w := range s.open {
		if w.spilled && w.n > 0 && w.tmin <= now && w.tmax > cut {
			return m.liveExact(s, now)
		}
	}
	// Open rows up to now: the newest per journey, and the trail rows, deduped (for
	// equal (journey, t) the last arrival wins). One pass; only trail rows are sorted.
	newest := map[string]*row{}
	var cand []*row
	consider := func(r *row) {
		if cur := newest[r.meta.ID]; cur == nil || r.t > cur.t || (r.t == cur.t && r.seq > cur.seq) {
			newest[r.meta.ID] = r
		}
	}
	for _, w := range s.open {
		if w.spilled {
			if w.tmin <= now {
				for _, r := range w.last {
					consider(r) // every row of the window is at or before cut
				}
			}
			continue
		}
		for k := range w.rows {
			r := &w.rows[k]
			if r.t > now {
				continue
			}
			consider(r)
			if r.t > cut {
				cand = append(cand, r)
			}
		}
	}
	sortByJourney(cand)
	return s.livePayload(now, newest, dedupe(cand)), nil
}

// livePayload assembles the live payload at now from the trail rows in (now - LiveTrail,
// now], the open rows in that range (deduplicated, sorted by journey and t), the newest
// open row at or before now of every journey, and the live map.
func (s *surface) livePayload(now core.TimeMs, newest map[string]*row, recent []*row) *core.MoverWindow {
	cut := now - core.LiveTrail
	var rows []*row
	inTrail := map[string]bool{}
	i := sort.Search(len(s.trail), func(i int) bool { return s.trail[i].t > cut })
	for ; i < len(s.trail) && s.trail[i].t <= now; i++ {
		rows = append(rows, s.trail[i])
		inTrail[s.trail[i].meta.ID] = true
	}
	for _, r := range recent {
		rows = append(rows, r)
		inTrail[r.meta.ID] = true
	}
	for id, r := range s.live {
		if _, ok := newest[id]; !ok && !inTrail[id] {
			rows = append(rows, r)
		}
	}
	for id, r := range newest {
		if !inTrail[id] && !r.gone() {
			rows = append(rows, r)
		}
	}
	return buildWindow(s.key, core.WindowLive, now, core.LiveTrail, firstWithBody(rows))
}

// liveExact is liveFromMemory when a window the payload's trail overlaps was evicted. It
// reads back only the windows that hold rows in (now - LiveTrail, now], evicted ones from
// their logs (which keep no bodies), and keeps of them the trail rows and the trains'
// rows up to now, whose bodies it follows again in time order; of the other rows only
// each journey's newest. The windows before those give each journey's newest row from
// their rows or index, and a train's row there, with its body, begins its RailBody as a
// keyframe row begins one (reseed). So a request holds the trail and the trains' rows of
// at most two windows, however many windows a held horizon keeps open. Callers hold m.mu
// for reading.
func (m *Store) liveExact(s *surface, now core.TimeMs) (*core.MoverWindow, error) {
	cut := now - core.LiveTrail
	starts := make([]core.TimeMs, 0, len(s.open))
	for ws, w := range s.open {
		if w.n > 0 && w.tmin <= now {
			starts = append(starts, ws)
		}
	}
	// Windows do not overlap in time: a later window's rows are newer.
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	newest := map[string]*row{}
	newer := func(r *row) bool {
		cur := newest[r.meta.ID]
		return cur == nil || r.t > cur.t || (r.t == cur.t && r.seq > cur.seq)
	}
	consider := func(r *row) {
		if newer(r) {
			newest[r.meta.ID] = r
		}
	}
	own := map[string]*row{} // copies of rows read back that newest points to
	var all []row
	for _, ws := range starts {
		w := s.open[ws]
		if w.tmax <= cut {
			// before the trail
			if w.spilled {
				for _, r := range w.last {
					consider(r)
				}
			} else {
				for i := range w.rows {
					consider(&w.rows[i])
				}
			}
			continue
		}
		take := func(r *row, resident bool) {
			switch {
			case r.t > now:
			case r.t > cut || r.meta.Kind == core.KindTrain:
				all = append(all, *r)
			case resident:
				consider(r)
			case newer(r):
				c := own[r.meta.ID]
				if c == nil {
					c = new(row)
					own[r.meta.ID] = c
				}
				*c = *r
				newest[r.meta.ID] = c
			}
		}
		if !w.spilled {
			for i := range w.rows {
				take(&w.rows[i], true)
			}
			continue
		}
		var seq uint64
		if err := m.eachWinRow(s, w, func(r row) {
			seq++
			r.seq = seq
			take(&r, false)
		}); err != nil {
			return nil, err
		}
		m.liveReadBacks.Add(1)
	}
	ptrs := make([]*row, len(all))
	for i := range all {
		ptrs[i] = &all[i]
	}
	sortByJourney(ptrs)
	// A train with rows in the earlier windows begins at its newest row there; the others
	// at the state at the horizon (followRows clones what it takes from base).
	seeds := rails{}
	for _, r := range ptrs {
		if b := newest[r.meta.ID]; b != nil && r.meta.Kind == core.KindTrain && seeds[r.meta.ID] == nil {
			nb := new(core.RailBody)
			nb.Seed(b.withBody().railRow())
			seeds[r.meta.ID] = nb
		}
	}
	base := s.rails
	if len(seeds) > 0 {
		base = make(rails, len(s.rails)+len(seeds))
		for id, b := range s.rails {
			base[id] = b
		}
		for id, b := range seeds {
			base[id] = b
		}
	}
	followRows(rails{}, base, ptrs)
	ptrs = dedupe(ptrs)
	var recent []*row
	for _, r := range ptrs {
		if r.t > cut {
			recent = append(recent, r)
		}
		consider(r)
	}
	return s.livePayload(now, newest, recent), nil
}

// seedRails returns the rail path state a keyframe gives: each train's RailBody begun at its
// keyframe row, which lists the pieces under the train (core.RailBody.Seed).
func seedRails(w *core.MoverWindow) rails {
	rs := rails{}
	for i := range w.Rows {
		meta := &w.Movers[w.Rows[i].Mover]
		if meta.Kind == core.KindTrain {
			r := rowFromCore(meta, &w.Rows[i])
			rs.get(meta.ID, nil).Seed(r.railRow())
		}
	}
	return rs
}

// reseed restarts every journey's RailBody at its row in live as a keyframe holds it, so
// bodies after a keyframe depend on the rows since it only, whether or not the store was
// reopened from it in between (loadSurface).
func reseed(live map[string]*row) rails {
	rs := rails{}
	for id, r := range live {
		if r.meta.Kind == core.KindTrain {
			rs.get(id, nil).Seed(r.withBody().railRow())
		}
	}
	return rs
}

// firstWithBody replaces each journey's first row in rows by the row with its body
// (row.withBody): the live payload's first row of a moving train places its whole body.
func firstWithBody(rows []*row) []*row {
	first := map[string]int{}
	for i, r := range rows {
		if j, ok := first[r.meta.ID]; !ok || r.t < rows[j].t {
			first[r.meta.ID] = i
		}
	}
	for _, i := range first {
		rows[i] = rows[i].withBody()
	}
	return rows
}

// liveFromDisk rebuilds the live payload at a closed instant now from the keyframe at
// or before now - LiveTrail and the closed windows up to now.
func (s *surface) liveFromDisk(m *Store, now, first core.TimeMs, hasData bool) (*core.MoverWindow, error) {
	cut := now - core.LiveTrail
	last := map[string]*row{}
	var trail []*row
	rs := rails{}
	if hasData && now >= first {
		from := core.WindowStart(cut, core.MoverWindowLen)
		kf := core.WindowStart(from, core.MoverKeyframeEvery)
		for ; kf > first; kf -= core.MoverKeyframeEvery {
			b, err := readWindowFile(s.keyframePath(kf))
			if errors.Is(err, core.ErrNoVersion) {
				continue
			}
			if err != nil {
				return nil, err
			}
			w, err := decodeFile(b, s.key)
			if err != nil {
				m.damaged(s.keyframePath(kf), err)
				continue
			}
			for i := range w.Rows {
				r := rowFromCore(&w.Movers[w.Rows[i].Mover], &w.Rows[i])
				last[r.meta.ID] = &r
			}
			rs = seedRails(w)
			break
		}
		if kf <= first {
			kf = core.WindowStart(first, core.MoverWindowLen)
		}
		for ws := kf; ws <= now; ws += core.MoverWindowLen {
			if ws > kf && ws%core.MoverKeyframeEvery == 0 {
				// bodies after a keyframe follow on from its rows, as the store that wrote it did
				if w, err := s.readWindow(s.keyframePath(ws)); err == nil {
					rs = seedRails(w)
				}
			}
			b, err := readWindowFile(s.windowPath(ws))
			if errors.Is(err, core.ErrNoVersion) {
				continue
			}
			if err != nil {
				return nil, err
			}
			w, err := decodeFile(b, s.key)
			if err != nil {
				m.damaged(s.windowPath(ws), err)
				continue
			}
			rows := make([]*row, 0, len(w.Rows))
			for i := range w.Rows {
				if w.Rows[i].T > now {
					continue
				}
				r := rowFromCore(&w.Movers[w.Rows[i].Mover], &w.Rows[i])
				rows = append(rows, &r)
			}
			sortByJourney(rows)
			for _, rp := range rows {
				rs.add(rp, nil)
				if cur := last[rp.meta.ID]; cur == nil || cur.t < rp.t {
					last[rp.meta.ID] = rp
				}
				if rp.t > cut {
					trail = append(trail, rp)
				}
			}
		}
	}
	rows := trail
	inTrail := map[string]bool{}
	for _, r := range trail {
		inTrail[r.meta.ID] = true
	}
	for id, r := range last {
		if !inTrail[id] && !r.gone() {
			rows = append(rows, r)
		}
	}
	return buildWindow(s.key, core.WindowLive, now, core.LiveTrail, firstWithBody(rows)), nil
}

// Stats returns the current counters and gauges.
func (m *Store) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.stats
	st.OpenRows = m.openRows
	st.OpenBytes = m.resident
	st.Corrupt = m.corrupt.Load()
	st.LiveReadBacks = m.liveReadBacks.Load()
	for _, s := range m.sfc {
		st.TrailRows += len(s.trail)
		st.LiveMovers += len(s.live)
		for _, w := range s.open {
			if w.spilled {
				st.SpilledWindows++
			}
		}
	}
	return st
}

// Flush fsyncs the window logs now (for SyncInterval > 0).
func (m *Store) Flush() error {
	if m.opts.NoWAL {
		return nil
	}
	return m.syncLogs()
}

// Close implements core.MoverStore. Open-window samples stay in the window logs and are
// replayed by the next Open; they are not committed.
func (m *Store) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	if m.stop != nil {
		close(m.stop)
		<-m.done
	}
	if !m.opts.NoWAL {
		return m.closeLogs()
	}
	return nil
}
