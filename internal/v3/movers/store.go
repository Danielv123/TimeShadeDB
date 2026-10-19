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

// ErrOverloaded is returned by Append when accepting the batch would exceed
// Options.MaxOpenRows. Nothing of the batch is applied; the caller retries after the
// next Commit (the HTTP ingest answers 503). It wraps core.ErrOverloaded.
var ErrOverloaded = fmt.Errorf("movers: open-window row limit reached: %w", core.ErrOverloaded)

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
	// MaxOpenRows bounds the samples held in open windows across all surfaces
	// (default 1,048,576, about 75 MB). Append returns ErrOverloaded beyond it.
	MaxOpenRows int
	// MaxTrailRows bounds, per surface, the closed rows kept for Live (the last
	// core.LiveTrail before the horizon; default 262,144). When exceeded the oldest
	// are dropped, so Live may miss trail rows (never last states).
	MaxTrailRows int
	// MaxLiveMovers bounds, per surface, the journeys tracked as live (last row not a
	// gone; default 131,072). Beyond it the journeys with the oldest last row are
	// forgotten, as if they had a gone: they are left out of keyframes and Live.
	MaxLiveMovers int
	// MaxAhead bounds how far after the reference clock a sample may be (default 1
	// hour; < 0 disables). The reference is the later of the latest Commit upTo (at
	// Open: the newest horizon) and Clock(). Append counts later samples as Invalid
	// (Stats.Ahead), and Commit drops open rows beyond it that were accepted while there
	// was no reference yet. Without the bound a sample from a broken clock would stay
	// open forever and hold MaxOpenRows.
	MaxAhead core.TimeMs
	// MaxBehind bounds how far before the reference clock a sample may be on a surface
	// that has no horizon yet (default 1 day; < 0 disables). Append counts older
	// samples as Late (Stats.Behind), and Commit drops such open rows, so a new surface
	// cannot start years back and make one Commit write a keyframe for every hour since.
	// Surfaces with a horizon reject everything before it anyway. Bulk imports that
	// load one surface after another must disable it.
	MaxBehind core.TimeMs
	// Clock, if set, returns the map clock (the server's wall clock in ms). It provides
	// the MaxAhead/MaxBehind reference before the first Commit after Open.
	Clock func() core.TimeMs
	// Logf receives warnings about damaged files skipped at Open or in Live and about
	// WAL frames skipped at replay (default log.Printf).
	Logf func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.MaxAhead == 0 {
		o.MaxAhead = core.Hour
	}
	if o.MaxBehind == 0 {
		o.MaxBehind = core.Day
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.MaxOpenRows <= 0 {
		o.MaxOpenRows = 1 << 20
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
	openRows int
	seq      uint64
	wal      *wal
	stats    Stats
	// upTo is the latest Commit upTo (at Open, the newest horizon); hasUpTo is false
	// until either exists.
	upTo    core.TimeMs
	hasUpTo bool
	// corrupt counts damaged window and keyframe files skipped (Live updates it without
	// the write lock).
	corrupt atomic.Int64

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
	OpenRows                int   // gauge
	TrailRows               int   // gauge
	LiveMovers              int   // gauge
	Evicted                 int64 // live journeys forgotten by MaxLiveMovers
	// Ahead and Behind count samples outside Options.MaxAhead/MaxBehind, rejected by
	// Append (as Invalid and Late) or dropped from the open rows by Commit.
	Ahead, Behind int64
	// Corrupt counts window and keyframe files that did not decode and were skipped
	// while rebuilding live state (Open, and Live before the horizon).
	Corrupt int64
	// WALSkipped counts WAL frames with a valid CRC that did not decode at Open.
	WALSkipped int64
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

	// open holds the accepted rows with t >= horizon, in arrival order, possibly with
	// duplicate (journey, t); the last arrival wins.
	open []row
	// live is the last closed row of every journey whose last closed row is not a gone:
	// the state at the horizon.
	live map[string]*row
	// trail holds the closed rows with t > horizon - LiveTrail, sorted by t.
	trail []*row
	// metas interns MoverMeta values.
	metas map[core.MoverMeta]*core.MoverMeta
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
	m := &Store{opts: opts, dir: filepath.Join(db, "movers"), sfc: map[core.SurfaceKey]*surface{}}
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
	if !opts.NoWAL {
		w, err := openWAL(filepath.Join(m.dir, "wal"), opts.SyncInterval)
		if err != nil {
			return nil, err
		}
		err = w.replay(func(key core.SurfaceKey, r row) {
			s := m.sfc[key]
			if s == nil {
				if key.Validate() != nil {
					return
				}
				s = m.newSurface(key)
				m.sfc[key] = s
			}
			if s.hasHorizon && r.t < s.horizon {
				return // committed before the crash
			}
			r.meta = s.intern(r.meta)
			m.seq++
			r.seq = m.seq
			s.open = append(s.open, r)
			s.observe(r.t)
			m.openRows++
		})
		if err != nil {
			return nil, fmt.Errorf("movers: wal: %w", err)
		}
		if w.skipped > 0 {
			m.stats.WALSkipped = int64(w.skipped)
			opts.Logf("movers: wal: skipped %d frames that did not decode", w.skipped)
		}
		m.wal = w
		if opts.SyncInterval > 0 {
			m.stop, m.done = make(chan struct{}), make(chan struct{})
			go m.syncLoop()
		}
	}
	return m, nil
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
			m.wal.sync()
		}
	}
}

func (m *Store) newSurface(key core.SurfaceKey) *surface {
	id := key.ID()
	return &surface{
		key:   key,
		id:    id,
		dir:   filepath.Join(m.dir, id),
		live:  map[string]*row{},
		metas: map[core.MoverMeta]*core.MoverMeta{},
		days:  map[string]*core.DayIndex{},
	}
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

// Append implements core.MoverStore. The batch is checked and logged to the WAL before
// anything changes in memory: when the WAL write fails nothing is applied and the
// result is zero.
func (m *Store) Append(ctx context.Context, samples []core.MoverSample) (core.ApplyResult, error) {
	var res core.ApplyResult
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return res, core.ErrClosed
	}
	if m.wal != nil {
		if err := m.wal.check(); err != nil {
			return res, fmt.Errorf("movers: wal: %w", err)
		}
	}
	ref, hasRef := m.ref()
	var ahead, behind int64
	type pending struct {
		s   *surface // nil: created when the batch is applied
		key core.SurfaceKey
		r   row
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
		if hasRef && m.opts.MaxBehind >= 0 && (s == nil || !s.hasHorizon) && smp.T < ref-m.opts.MaxBehind {
			res.Late++
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
	if m.openRows+len(accepted) > m.opts.MaxOpenRows {
		return core.ApplyResult{}, fmt.Errorf("%w (%d open, batch %d, limit %d)", ErrOverloaded, m.openRows, len(accepted), m.opts.MaxOpenRows)
	}
	if m.wal != nil && len(accepted) > 0 {
		logged := make([]walEntry, len(accepted))
		for i := range accepted {
			logged[i] = walEntry{sfc: accepted[i].key, r: &accepted[i].r}
		}
		if err := m.wal.append(logged); err != nil {
			return core.ApplyResult{}, fmt.Errorf("movers: wal: %w", err)
		}
	}
	for i := range accepted {
		p := &accepted[i]
		s := p.s
		if s == nil {
			if s = m.sfc[p.key]; s == nil {
				s = m.newSurface(p.key)
				m.sfc[p.key] = s
			}
		}
		m.seq++
		p.r.seq = m.seq
		p.r.meta = s.intern(p.r.meta)
		s.open = append(s.open, p.r)
		s.observe(p.r.t)
	}
	m.openRows += len(accepted)
	res.Accepted = len(accepted)
	m.stats.Accepted += int64(res.Accepted)
	m.stats.Late += int64(res.Late)
	m.stats.Invalid += int64(res.Invalid)
	m.stats.Ahead += ahead
	m.stats.Behind += behind
	return res, nil
}

// Commit implements core.MoverStore.
func (m *Store) Commit(ctx context.Context, upTo core.TimeMs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return core.ErrClosed
	}
	newH := core.WindowStart(upTo, core.MoverWindowLen)
	if !m.hasUpTo || upTo > m.upTo {
		m.upTo, m.hasUpTo = upTo, true
	}
	keys := make([]core.SurfaceKey, 0, len(m.sfc))
	for k := range m.sfc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID() < keys[j].ID() })
	changed := m.dropOutOfRange(keys)
	for _, k := range keys {
		s := m.sfc[k]
		if s.hasHorizon && newH <= s.horizon {
			continue
		}
		n, err := m.commitSurface(s, newH)
		if err != nil {
			return fmt.Errorf("movers: commit %s: %w", s.id, err)
		}
		m.openRows -= n
		changed = true
	}
	if changed && m.wal != nil {
		var entries []walEntry
		for _, k := range keys {
			s := m.sfc[k]
			for i := range s.open {
				entries = append(entries, walEntry{sfc: s.key, r: &s.open[i]})
			}
		}
		if err := m.wal.rewrite(entries); err != nil {
			return fmt.Errorf("movers: wal rewrite: %w", err)
		}
	}
	return nil
}

// dropOutOfRange removes open rows outside Options.MaxAhead/MaxBehind of the reference
// clock. Append rejects such samples once a reference exists, so this only finds rows
// accepted before the first Commit (or Clock) of a fresh store. It reports whether it
// removed any.
func (m *Store) dropOutOfRange(keys []core.SurfaceKey) bool {
	ref, ok := m.ref()
	if !ok {
		return false
	}
	dropped := false
	for _, k := range keys {
		s := m.sfc[k]
		keep := s.open[:0]
		for _, r := range s.open {
			switch {
			case m.opts.MaxAhead >= 0 && r.t > ref+m.opts.MaxAhead:
				m.stats.Ahead++
			case m.opts.MaxBehind >= 0 && !s.hasHorizon && r.t < ref-m.opts.MaxBehind:
				m.stats.Behind++
			default:
				keep = append(keep, r)
			}
		}
		if n := len(s.open) - len(keep); n > 0 {
			clear(s.open[len(keep):])
			s.open = keep
			m.openRows -= n
			dropped = true
			if !s.hasHorizon {
				// first and last came from the open rows alone.
				s.hasData = false
				for i := range s.open {
					s.observe(s.open[i].t)
				}
			}
		}
	}
	return dropped
}

// commitSurface closes the windows of s that end at or before newH and returns the
// number of open rows it removed. The new live state and trail are built on copies and
// installed, with the new horizon, only once meta.json is written: after a failure the
// surface is unchanged and the Commit can be retried (files already written are written
// again with the same or newer content).
func (m *Store) commitSurface(s *surface, newH core.TimeMs) (int, error) {
	sync := m.opts.SyncInterval >= 0
	before := len(s.open)
	live := maps.Clone(s.live)
	// Dedupe the open rows: for equal (journey, t) the last arrival wins.
	ptrs := make([]*row, len(s.open))
	for i := range s.open {
		ptrs[i] = &s.open[i]
	}
	sortByJourney(ptrs)
	ptrs = dedupe(ptrs)
	// Handoffs: a gone followed by a sample of the same journey within HandoffWindow
	// among all samples known now.
	for i, r := range ptrs {
		if r.t >= newH || !r.gone() {
			continue
		}
		for j := i + 1; j < len(ptrs) && ptrs[j].meta.ID == r.meta.ID && ptrs[j].t <= r.t+core.HandoffWindow; j++ {
			if !ptrs[j].gone() && ptrs[j].t > r.t {
				r.flags |= core.FlagHandoff
				break
			}
		}
	}
	// Partition into closing windows and rows that stay open.
	byWindow := map[core.TimeMs][]*row{}
	var keep []row
	for _, r := range ptrs {
		if r.t < newH {
			ws := core.WindowStart(r.t, core.MoverWindowLen)
			byWindow[ws] = append(byWindow[ws], r)
		} else {
			keep = append(keep, *r)
		}
	}
	starts := make([]core.TimeMs, 0, len(byWindow))
	for ws := range byWindow {
		starts = append(starts, ws)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	// Keyframes at every UTC hour h with oldH < h <= newH and first < h.
	var h core.TimeMs = math.MaxInt64
	if s.hasData {
		h = core.WindowStart(s.first, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
		if s.hasHorizon && h <= s.horizon {
			h = core.WindowStart(s.horizon, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
		}
	}
	touched := map[string]bool{}
	emitKeyframes := func(upTo core.TimeMs) error {
		for ; h <= newH && h <= upTo; h += core.MoverKeyframeEvery {
			if err := m.writeKeyframe(s, h, live, sync); err != nil {
				return err
			}
			touched[core.UTCDay(h)] = true
		}
		return nil
	}
	var closedRows []*row
	for _, ws := range starts {
		if err := emitKeyframes(ws); err != nil {
			return 0, err
		}
		rows := byWindow[ws]
		w := buildWindow(s.key, core.WindowClosed, ws, core.MoverWindowLen, rows)
		body, err := wire.EncodeMoverWindow(w)
		if err != nil {
			return 0, err
		}
		file, err := encodeWindowFile(body)
		if err != nil {
			return 0, err
		}
		if err := writeFileAtomic(s.windowPath(ws), file, sync); err != nil {
			return 0, err
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
			return 0, err
		}
		i := sort.Search(len(day.Windows), func(i int) bool { return day.Windows[i].Start >= ws })
		if i < len(day.Windows) && day.Windows[i].Start == ws {
			day.Windows[i] = ref
		} else {
			day.Windows = append(day.Windows, core.WindowRef{})
			copy(day.Windows[i+1:], day.Windows[i:])
			day.Windows[i] = ref
		}
		touched[core.UTCDay(ws)] = true
		advance(live, rows)
		closedRows = append(closedRows, rows...)
	}
	if err := emitKeyframes(newH); err != nil {
		return 0, err
	}
	// Trail: closed rows with t > newH - LiveTrail.
	cut := newH - core.LiveTrail
	trail := make([]*row, 0, len(s.trail)+len(closedRows))
	for _, r := range s.trail {
		if r.t > cut {
			trail = append(trail, r)
		}
	}
	for _, r := range closedRows {
		if r.t > cut {
			c := *r
			trail = append(trail, &c)
		}
	}
	sort.SliceStable(trail, func(i, j int) bool { return trail[i].t < trail[j].t })
	if len(trail) > m.opts.MaxTrailRows {
		trail = append([]*row(nil), trail[len(trail)-m.opts.MaxTrailRows:]...)
	}
	evicted := m.evictLive(live)
	// Day indexes, then the surface meta (the horizon) last.
	for d := range touched {
		if err := s.writeDay(d, sync); err != nil {
			return 0, err
		}
	}
	if err := s.writeMeta(true, newH, sync); err != nil {
		return 0, err
	}
	s.hasHorizon, s.horizon = true, newH
	s.live, s.trail = live, trail
	m.stats.Evicted += evicted
	s.open = keep
	// Drop cached day indexes that can no longer change.
	today := core.UTCDay(newH)
	for d := range s.days {
		if d < today {
			delete(s.days, d)
		}
	}
	s.reintern()
	return before - len(keep), nil
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

// evictLive applies Options.MaxLiveMovers to a live map and returns how many journeys
// it forgot.
func (m *Store) evictLive(live map[string]*row) int64 {
	if len(live) <= m.opts.MaxLiveMovers {
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
	n := len(all) - m.opts.MaxLiveMovers
	for _, r := range all[:n] {
		delete(live, r.meta.ID)
	}
	return int64(n)
}

// reintern rebuilds the meta table from the rows still referenced, so it stays bounded.
func (s *surface) reintern() {
	metas := make(map[core.MoverMeta]*core.MoverMeta, len(s.live)+len(s.open))
	keep := func(p *core.MoverMeta) {
		if _, ok := metas[*p]; !ok {
			metas[*p] = p
		}
	}
	for _, r := range s.live {
		keep(r.meta)
	}
	for i := range s.open {
		keep(s.open[i].meta)
	}
	for _, r := range s.trail {
		keep(r.meta)
	}
	s.metas = metas
}

func (m *Store) writeKeyframe(s *surface, h core.TimeMs, live map[string]*row, sync bool) error {
	rows := make([]*row, 0, len(live))
	for _, r := range live {
		rows = append(rows, r)
	}
	w := buildWindow(s.key, core.WindowKeyframe, h, 0, rows)
	body, err := wire.EncodeMoverWindow(w)
	if err != nil {
		return err
	}
	file, err := encodeWindowFile(body)
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
		from = h
		break
	}
	// Windows before the keyframe are read only for the trail.
	cut := s.horizon - core.LiveTrail
	start := min(from, core.WindowStart(cut+1, core.MoverWindowLen))
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
			if r.t > cut {
				c := r
				s.trail = append(s.trail, &c)
			}
		}
		if ws >= from {
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
			advance(s.live, rows)
		}
	}
	sort.SliceStable(s.trail, func(i, j int) bool { return s.trail[i].t < s.trail[j].t })
	if len(s.trail) > m.opts.MaxTrailRows {
		s.trail = s.trail[len(s.trail)-m.opts.MaxTrailRows:]
	}
	m.stats.Evicted += m.evictLive(s.live)
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
		return s.liveFromMemory(now), nil
	}
	first, hasData := s.first, s.hasData
	m.mu.RUnlock()
	return s.liveFromDisk(m, now, first, hasData)
}

func (s *surface) liveFromMemory(now core.TimeMs) *core.MoverWindow {
	cut := now - core.LiveTrail
	var rows []*row
	inTrail := map[string]bool{}
	i := sort.Search(len(s.trail), func(i int) bool { return s.trail[i].t > cut })
	for ; i < len(s.trail) && s.trail[i].t <= now; i++ {
		rows = append(rows, s.trail[i])
		inTrail[s.trail[i].meta.ID] = true
	}
	// Open rows up to now: the newest per journey, and the trail rows, deduped (for
	// equal (journey, t) the last arrival wins). One pass; only trail rows are sorted.
	newest := map[string]*row{}
	var cand []*row
	for k := range s.open {
		r := &s.open[k]
		if r.t > now {
			continue
		}
		if cur := newest[r.meta.ID]; cur == nil || r.t > cur.t || (r.t == cur.t && r.seq > cur.seq) {
			newest[r.meta.ID] = r
		}
		if r.t > cut {
			cand = append(cand, r)
		}
	}
	sortByJourney(cand)
	for _, r := range dedupe(cand) {
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
	return buildWindow(s.key, core.WindowLive, now, core.LiveTrail, rows)
}

// liveFromDisk rebuilds the live payload at a closed instant now from the keyframe at
// or before now - LiveTrail and the closed windows up to now.
func (s *surface) liveFromDisk(m *Store, now, first core.TimeMs, hasData bool) (*core.MoverWindow, error) {
	cut := now - core.LiveTrail
	last := map[string]*row{}
	var trail []*row
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
			break
		}
		if kf <= first {
			kf = core.WindowStart(first, core.MoverWindowLen)
		}
		for ws := kf; ws <= now; ws += core.MoverWindowLen {
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
			for i := range w.Rows {
				if w.Rows[i].T > now {
					continue
				}
				r := rowFromCore(&w.Movers[w.Rows[i].Mover], &w.Rows[i])
				rp := &r
				if cur := last[r.meta.ID]; cur == nil || cur.t < r.t {
					last[r.meta.ID] = rp
				}
				if r.t > cut {
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
	return buildWindow(s.key, core.WindowLive, now, core.LiveTrail, rows), nil
}

// Stats returns the current counters and gauges.
func (m *Store) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.stats
	st.OpenRows = m.openRows
	st.Corrupt = m.corrupt.Load()
	for _, s := range m.sfc {
		st.TrailRows += len(s.trail)
		st.LiveMovers += len(s.live)
	}
	return st
}

// Flush fsyncs the WAL now (for SyncInterval > 0).
func (m *Store) Flush() error {
	if m.wal == nil {
		return nil
	}
	return m.wal.sync()
}

// Close implements core.MoverStore. Open-window samples stay in the WAL and are
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
	if m.wal != nil {
		return m.wal.close()
	}
	return nil
}
