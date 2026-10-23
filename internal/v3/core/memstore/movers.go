package memstore

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
)

// Movers is the reference core.MoverStore.
type Movers struct {
	mu     sync.RWMutex
	closed bool
	sfc    map[core.SurfaceKey]*moverSurface
}

var _ core.MoverStore = (*Movers)(nil)

type moverRow struct {
	s       core.MoverSample // quantized
	arrival uint64
}

type idT struct {
	id string
	t  core.TimeMs
}

type moverSurface struct {
	key       core.SurfaceKey
	horizon   core.TimeMs
	first     core.TimeMs
	last      core.TimeMs
	hasData   bool
	rows      []*moverRow
	byIDT     map[idT]*moverRow
	closed    map[core.TimeMs]*core.MoverWindow
	keyframes map[core.TimeMs]*core.MoverWindow
	arrivals  uint64

	// byID lists each journey's rows sorted by T. It only speeds up handoff detection;
	// the semantics are those of a scan over rows.
	byID map[string][]*moverRow
}

// NewMovers returns an empty store.
func NewMovers() *Movers {
	return &Movers{sfc: map[core.SurfaceKey]*moverSurface{}}
}

// Append implements core.MoverStore.
func (m *Movers) Append(ctx context.Context, samples []core.MoverSample) (core.ApplyResult, error) {
	var res core.ApplyResult
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return res, core.ErrClosed
	}
	for _, s := range samples {
		if s.Surface.Validate() != nil || s.Meta.ID == "" || !s.Meta.Kind.Valid() {
			res.Invalid++
			continue
		}
		sf := m.sfc[s.Surface]
		if sf == nil {
			sf = &moverSurface{
				key:       s.Surface,
				horizon:   noHorizon,
				byIDT:     map[idT]*moverRow{},
				byID:      map[string][]*moverRow{},
				closed:    map[core.TimeMs]*core.MoverWindow{},
				keyframes: map[core.TimeMs]*core.MoverWindow{},
			}
			m.sfc[s.Surface] = sf
		}
		if s.T < sf.horizon {
			res.Late++
			continue
		}
		q := s.Quantized()
		q.Flags &^= core.FlagHandoff // assigned at commit
		sf.arrivals++
		k := idT{q.Meta.ID, q.T}
		if old := sf.byIDT[k]; old != nil {
			old.s, old.arrival = q, sf.arrivals
		} else {
			r := &moverRow{s: q, arrival: sf.arrivals}
			sf.rows = append(sf.rows, r)
			sf.byIDT[k] = r
			list := append(sf.byID[k.id], r)
			for i := len(list) - 1; i > 0 && list[i-1].s.T > r.s.T; i-- {
				list[i-1], list[i] = list[i], list[i-1]
			}
			sf.byID[k.id] = list
		}
		if !sf.hasData || q.T < sf.first {
			sf.first = q.T
		}
		if !sf.hasData || q.T > sf.last {
			sf.last = q.T
		}
		sf.hasData = true
		res.Accepted++
	}
	return res, nil
}

// Commit implements core.MoverStore.
func (m *Movers) Commit(ctx context.Context, upTo core.TimeMs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return core.ErrClosed
	}
	newH := core.WindowStart(upTo, core.MoverWindowLen)
	for _, sf := range m.sfc {
		if newH <= sf.horizon {
			continue
		}
		oldH := sf.horizon
		closing := map[core.TimeMs][]*moverRow{}
		for _, r := range sf.rows {
			if r.s.T >= oldH && r.s.T < newH {
				ws := core.WindowStart(r.s.T, core.MoverWindowLen)
				closing[ws] = append(closing[ws], r)
			}
		}
		// Handoffs: a gone followed by a sample of the same journey within HandoffWindow.
		for _, rows := range closing {
			for _, g := range rows {
				if g.s.Flags&core.FlagGone == 0 {
					continue
				}
				list := sf.byID[g.s.Meta.ID]
				i := sort.Search(len(list), func(i int) bool { return list[i].s.T > g.s.T })
				for ; i < len(list) && list[i].s.T <= g.s.T+core.HandoffWindow; i++ {
					if list[i].s.Flags&core.FlagGone == 0 {
						g.s.Flags |= core.FlagHandoff
						break
					}
				}
			}
		}
		for ws, rows := range closing {
			sf.closed[ws] = buildWindow(sf.key, core.WindowClosed, ws, core.MoverWindowLen, rows, nil)
		}
		if sf.hasData {
			h := core.WindowStart(sf.first, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
			if oldH != noHorizon && h <= oldH {
				h = core.WindowStart(oldH, core.MoverKeyframeEvery) + core.MoverKeyframeEvery
			}
			for ; h <= newH; h += core.MoverKeyframeEvery {
				rows := sf.lastRows(h, false, true)
				sf.keyframes[h] = buildWindow(sf.key, core.WindowKeyframe, h, 0, rows, sf.bodies(rows))
			}
		}
		sf.horizon = newH
	}
	return nil
}

// lastRows returns, per journey, the newest row with T < before (inclusive: T <= before
// when inclusive is set). Journeys whose newest row is a gone are skipped when liveOnly.
func (sf *moverSurface) lastRows(before core.TimeMs, inclusive, liveOnly bool) []*moverRow {
	last := map[string]*moverRow{}
	for _, r := range sf.rows {
		if r.s.T > before || (!inclusive && r.s.T == before) {
			continue
		}
		if cur := last[r.s.Meta.ID]; cur == nil || r.s.T > cur.s.T {
			last[r.s.Meta.ID] = r
		}
	}
	out := make([]*moverRow, 0, len(last))
	for _, r := range last {
		if liveOnly && r.s.Flags&core.FlagGone != 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

// bodies returns the body (core.RailBody) of each given row: the rail pieces under the train
// that the journey's earlier rows listed, following on from the newest keyframe at or before
// the row (its row of the journey begins the RailBody, core.RailBody.Seed). Keyframes and
// the live payload put them in front of the first row's own pieces (core.MoverStore).
func (sf *moverSurface) bodies(rows []*moverRow) map[*moverRow][]core.RailPiece {
	out := map[*moverRow][]core.RailPiece{}
	for _, r := range rows {
		if r.s.Meta.Kind != core.KindTrain || r.s.Flags&core.FlagRail == 0 {
			continue // only rail rows have bodies
		}
		var rb core.RailBody
		from := core.TimeMs(math.MinInt64)
		for h, kf := range sf.keyframes {
			if h <= r.s.T && h > from {
				from = h
				rb = core.RailBody{}
				for _, x := range kf.Rows {
					if m := kf.Movers[x.Mover]; m.ID == r.s.Meta.ID && m.Kind == core.KindTrain {
						rb.Seed(core.RailRow{T: x.T, X: x.X, Y: x.Y, Orientation: x.Orientation, Flags: x.Flags, Rail: x.Rail,
							RailStart: x.RailStart, Carriages: m.Carriages})
					}
				}
			}
		}
		list := sf.byID[r.s.Meta.ID]
		i := sort.Search(len(list), func(i int) bool { return list[i].s.T >= from })
		for _, x := range list[i:] {
			if x.s.T > r.s.T {
				break
			}
			s := &x.s
			body := rb.Add(core.RailRow{T: s.T, X: s.X, Y: s.Y, Orientation: s.Orientation, Flags: s.Flags, Rail: s.Rail,
				RailStart: s.RailStart, Carriages: s.Meta.Carriages})
			if x == r && len(body) > 0 {
				out[r] = body
			}
		}
	}
	return out
}

// buildWindow makes a MoverWindow with the canonical table and row order: one entry per
// distinct meta, sorted by (ID, first row T), rows sorted by (entry, T). A row in bodies
// carries its body in front of its own rail pieces (core.RailWithBody).
func buildWindow(sfc core.SurfaceKey, kind core.WindowKind, start, length core.TimeMs, rows []*moverRow, bodies map[*moverRow][]core.RailPiece) *core.MoverWindow {
	type entry struct {
		meta  core.MoverMeta
		first core.TimeMs
	}
	entries := map[core.MoverMeta]*entry{}
	for _, r := range rows {
		e := entries[r.s.Meta]
		if e == nil {
			entries[r.s.Meta] = &entry{meta: r.s.Meta, first: r.s.T}
		} else if r.s.T < e.first {
			e.first = r.s.T
		}
	}
	list := make([]*entry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.meta.ID != b.meta.ID {
			return a.meta.ID < b.meta.ID
		}
		if a.first != b.first {
			return a.first < b.first
		}
		return fmt.Sprint(a.meta) < fmt.Sprint(b.meta)
	})
	index := map[core.MoverMeta]int{}
	w := &core.MoverWindow{Surface: sfc, Kind: kind, Start: start, Len: length, Movers: make([]core.MoverMeta, len(list))}
	for i, e := range list {
		w.Movers[i] = e.meta
		index[e.meta] = i
	}
	w.Rows = make([]core.MoverRow, 0, len(rows))
	for _, r := range rows {
		w.Rows = append(w.Rows, core.MoverRow{
			Mover:       index[r.s.Meta],
			T:           r.s.T,
			Tick:        r.s.Tick,
			X:           r.s.X,
			Y:           r.s.Y,
			Orientation: r.s.Orientation,
			Flags:       r.s.Flags,
			Carriages:   append([]core.Carriage(nil), r.s.Carriages...),
			Speed:       r.s.Speed,
			Rail:        append([]core.RailPiece(nil), core.RailWithBody(bodies[r], r.s.Rail)...),
			RailStart:   r.s.RailStart,
		})
	}
	sort.Slice(w.Rows, func(i, j int) bool {
		a, b := w.Rows[i], w.Rows[j]
		if a.Mover != b.Mover {
			return a.Mover < b.Mover
		}
		return a.T < b.T
	})
	return w
}

// CloneWindow returns a deep copy of w.
func CloneWindow(w *core.MoverWindow) *core.MoverWindow {
	c := *w
	c.Movers = append([]core.MoverMeta(nil), w.Movers...)
	c.Rows = make([]core.MoverRow, len(w.Rows))
	for i, r := range w.Rows {
		r.Carriages = append([]core.Carriage(nil), r.Carriages...)
		r.Rail = append([]core.RailPiece(nil), r.Rail...)
		c.Rows[i] = r
	}
	return &c
}

func (m *Movers) surface(sfc core.SurfaceKey) (*moverSurface, error) {
	if m.closed {
		return nil, core.ErrClosed
	}
	sf := m.sfc[sfc]
	if sf == nil {
		return nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, sfc)
	}
	return sf, nil
}

// Surfaces implements core.MoverStore.
func (m *Movers) Surfaces(ctx context.Context) ([]core.SurfaceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, core.ErrClosed
	}
	out := make([]core.SurfaceInfo, 0, len(m.sfc))
	for _, sf := range m.sfc {
		info := core.SurfaceInfo{Key: sf.key, ID: sf.key.ID(), Days: sf.days()}
		if sf.horizon != noHorizon {
			info.Horizon = sf.horizon
		}
		if sf.hasData {
			info.FirstT, info.LastT = sf.first, sf.last
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (sf *moverSurface) days() []string {
	set := map[string]bool{}
	for t := range sf.closed {
		set[core.UTCDay(t)] = true
	}
	for t := range sf.keyframes {
		set[core.UTCDay(t)] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Day implements core.MoverStore.
func (m *Movers) Day(ctx context.Context, sfc core.SurfaceKey, date string) (core.DayIndex, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sf, err := m.surface(sfc)
	if err != nil {
		return core.DayIndex{}, err
	}
	start, err := core.ParseUTCDay(date)
	if err != nil {
		return core.DayIndex{}, fmt.Errorf("%w: %v", core.ErrNoVersion, err)
	}
	idx := core.DayIndex{Date: date, Windows: []core.WindowRef{}, Keyframes: []core.TimeMs{}}
	for ws, w := range sf.closed {
		if core.UTCDay(ws) != date || len(w.Rows) == 0 {
			continue
		}
		ref := core.WindowRef{Start: ws, First: w.Rows[0].T, Last: w.Rows[0].T, Rows: len(w.Rows)}
		for _, r := range w.Rows {
			ref.First = min(ref.First, r.T)
			ref.Last = max(ref.Last, r.T)
		}
		idx.Windows = append(idx.Windows, ref)
	}
	for t := range sf.keyframes {
		if core.UTCDay(t) == date {
			idx.Keyframes = append(idx.Keyframes, t)
		}
	}
	if len(idx.Windows) == 0 && len(idx.Keyframes) == 0 {
		return core.DayIndex{}, fmt.Errorf("%w: %v has nothing on %s", core.ErrNoVersion, sfc, date)
	}
	sort.Slice(idx.Windows, func(i, j int) bool { return idx.Windows[i].Start < idx.Windows[j].Start })
	sort.Slice(idx.Keyframes, func(i, j int) bool { return idx.Keyframes[i] < idx.Keyframes[j] })
	idx.Final = sf.horizon != noHorizon && sf.horizon >= start+core.Day
	return idx, nil
}

// Window implements core.MoverStore.
func (m *Movers) Window(ctx context.Context, sfc core.SurfaceKey, start core.TimeMs) (*core.MoverWindow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sf, err := m.surface(sfc)
	if err != nil {
		return nil, err
	}
	w := sf.closed[start]
	if w == nil || len(w.Rows) == 0 {
		return nil, fmt.Errorf("%w: %v window %d", core.ErrNoVersion, sfc, start)
	}
	return CloneWindow(w), nil
}

// Keyframe implements core.MoverStore.
func (m *Movers) Keyframe(ctx context.Context, sfc core.SurfaceKey, t core.TimeMs) (*core.MoverWindow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sf, err := m.surface(sfc)
	if err != nil {
		return nil, err
	}
	w := sf.keyframes[t]
	if w == nil {
		return nil, fmt.Errorf("%w: %v keyframe %d", core.ErrNoVersion, sfc, t)
	}
	return CloneWindow(w), nil
}

// Live implements core.MoverStore.
func (m *Movers) Live(ctx context.Context, sfc core.SurfaceKey, now core.TimeMs) (*core.MoverWindow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sf, err := m.surface(sfc)
	if err != nil {
		return nil, err
	}
	var rows []*moverRow
	first := map[string]*moverRow{} // each journey's first row in the payload
	for _, r := range sf.rows {
		if r.s.T > now-core.LiveTrail && r.s.T <= now {
			rows = append(rows, r)
			if f := first[r.s.Meta.ID]; f == nil || r.s.T < f.s.T {
				first[r.s.Meta.ID] = r
			}
		}
	}
	for _, r := range sf.lastRows(now, true, true) {
		if first[r.s.Meta.ID] == nil {
			rows = append(rows, r)
			first[r.s.Meta.ID] = r
		}
	}
	firsts := make([]*moverRow, 0, len(first))
	for _, r := range first {
		firsts = append(firsts, r)
	}
	return buildWindow(sf.key, core.WindowLive, now, core.LiveTrail, rows, sf.bodies(firsts)), nil
}

// Close implements core.MoverStore.
func (m *Movers) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
