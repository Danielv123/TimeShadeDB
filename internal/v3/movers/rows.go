package movers

import (
	"fmt"
	"math"
	"sort"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

// storedFlags are the core.MoverFlags bits a store keeps. Bit 6 is wire-only
// (docs/v3/FORMAT.md "MVW3"), so it is cleared on Append.
const storedFlags = core.FlagStop | core.FlagGone | core.FlagHandoff | core.FlagEndSwitch | core.FlagNoTick | core.FlagNoHeading | core.FlagRail

// MaxCarriages is the most carriages one sample may carry (wire.MaxCarriages, which the
// MVW3 decoder enforces); samples with more are Invalid.
const MaxCarriages = wire.MaxCarriages

// carriage is a quantized core.Carriage (positions in 1/16 tile, orientation in u16 units).
type carriage struct {
	x, y int32
	o    uint16
}

// row is one quantized sample as the store keeps it in memory. meta is interned per
// surface, so rows of one journey with equal meta share the pointer.
type row struct {
	meta  *core.MoverMeta
	t     core.TimeMs
	tick  int64
	x, y  int32
	o     uint16
	flags core.MoverFlags
	// With core.FlagRail: the rail start, speed in core.SpeedUnits and the rail pieces.
	rstart bool
	speed  uint32
	car    []carriage
	rail   []core.RailPiece
	// seq is the arrival order; for equal (journey, t) the highest seq wins.
	seq uint64
	// body is the rail pieces under the train that earlier rows of the journey listed
	// (core.RailBody), put in front of rail when the row is a keyframe's or the first of its
	// journey in the live payload. Not stored with the row; shared, never modified.
	body []core.RailPiece
}

// rowBytes is the approximate heap cost of one row without carriages, for budgets.
const rowBytes = 120

// railRow is what core.RailBody needs of r.
func (r *row) railRow() core.RailRow {
	return core.RailRow{T: r.t, X: float64(r.x) / core.PosUnits, Y: float64(r.y) / core.PosUnits,
		Orientation: float64(r.o) / core.OrientUnits, Flags: r.flags, Rail: r.rail, RailStart: r.rstart,
		Carriages: r.meta.Carriages}
}

// withBody returns r as a keyframe or the live payload holds it: a copy with its body in
// front of its rail pieces, or r itself when it has none.
func (r *row) withBody() *row {
	if len(r.body) == 0 {
		return r
	}
	c := *r
	c.rail = core.RailWithBody(r.body, r.rail)
	c.body = nil
	return &c
}

// rails maps journeys to their core.RailBody. A journey without one has none yet: a
// fresh one, or a clone of base's (see get).
type rails map[string]*core.RailBody

// get returns id's RailBody, made from base's copy (or empty) when it has none.
func (rs rails) get(id string, base rails) *core.RailBody {
	if b := rs[id]; b != nil {
		return b
	}
	b := new(core.RailBody)
	if c := base[id]; c != nil {
		b = c.Clone()
	}
	rs[id] = b
	return b
}

// add gives train row r its body from its journey's RailBody (made from base's when missing).
func (rs rails) add(r *row, base rails) {
	r.body = nil
	if r.meta.Kind == core.KindTrain {
		r.body = rs.get(r.meta.ID, base).Add(r.railRow())
	}
}

// clone returns a deep copy.
func (rs rails) clone() rails {
	c := make(rails, len(rs))
	for id, b := range rs {
		c[id] = b.Clone()
	}
	return c
}

func (r *row) gone() bool { return r.flags&core.FlagGone != 0 }

// quantPos converts a coordinate to position units, rejecting values a Factorio map
// cannot have (NaN, infinities, beyond int32 units).
func quantPos(v float64) (int32, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	u := core.QuantizePos(v)
	if u < math.MinInt32 || u > math.MaxInt32 {
		return 0, false
	}
	return int32(u), true
}

// quantOrient is core.QuantizeOrientation with NaN and infinities mapped to 0.
func quantOrient(o float64) uint16 {
	if math.IsNaN(o) || math.IsInf(o, 0) {
		return 0
	}
	return core.QuantizeOrientation(o)
}

// makeRow quantizes s exactly as core.MoverSample.Quantized does. It reports false for
// coordinates outside the representable range and for more than MaxCarriages carriages. FlagHandoff is cleared (stores assign
// it at commit) and the tick of a no-tick sample is dropped (the wire format has no
// place for it).
func makeRow(s *core.MoverSample, meta *core.MoverMeta, seq uint64) (row, bool) {
	r := row{meta: meta, t: s.T, tick: s.Tick, flags: s.Flags & storedFlags &^ core.FlagHandoff, seq: seq}
	if r.flags&core.FlagNoTick != 0 {
		r.tick = 0
	}
	if r.gone() {
		return r, true
	}
	var okx, oky bool
	r.x, okx = quantPos(s.X)
	r.y, oky = quantPos(s.Y)
	if !okx || !oky {
		return r, false
	}
	r.o = quantOrient(s.Orientation)
	if len(s.Carriages) > MaxCarriages {
		return r, false
	}
	if r.flags&core.FlagRail != 0 {
		if s.Meta.Kind != core.KindTrain || len(s.Rail) > core.MaxRailPieces || math.IsNaN(s.Speed) {
			return r, false
		}
		r.speed = core.QuantizeSpeed(s.Speed)
		r.rstart = s.RailStart
		if len(s.Rail) > 0 {
			for _, p := range s.Rail {
				if !p.Valid() {
					return r, false
				}
			}
			r.rail = append([]core.RailPiece(nil), s.Rail...)
		}
	}
	if len(s.Carriages) > 0 {
		r.car = make([]carriage, len(s.Carriages))
		for i, c := range s.Carriages {
			cx, ok1 := quantPos(c.X)
			cy, ok2 := quantPos(c.Y)
			if !ok1 || !ok2 {
				return r, false
			}
			r.car[i] = carriage{x: cx, y: cy, o: quantOrient(c.Orientation)}
		}
	}
	return r, true
}

// coreRow converts r to a core.MoverRow for mover table entry i.
func (r *row) coreRow(i int) core.MoverRow {
	mr := core.MoverRow{Mover: i, T: r.t, Tick: r.tick, Flags: r.flags}
	if r.gone() {
		return mr
	}
	mr.X = float64(r.x) / core.PosUnits
	mr.Y = float64(r.y) / core.PosUnits
	mr.Orientation = float64(r.o) / core.OrientUnits
	if len(r.car) > 0 {
		mr.Carriages = make([]core.Carriage, len(r.car))
		for k, c := range r.car {
			mr.Carriages[k] = core.Carriage{X: float64(c.x) / core.PosUnits, Y: float64(c.y) / core.PosUnits, Orientation: float64(c.o) / core.OrientUnits}
		}
	}
	if r.flags&core.FlagRail != 0 {
		mr.Speed = float64(r.speed) / core.SpeedUnits
		mr.RailStart = r.rstart
		if len(r.rail) > 0 {
			mr.Rail = append([]core.RailPiece(nil), r.rail...)
		}
	}
	return mr
}

// rowFromCore is the inverse of coreRow for rows read back from disk (already quantized).
func rowFromCore(meta *core.MoverMeta, mr *core.MoverRow) row {
	r := row{meta: meta, t: mr.T, tick: mr.Tick, flags: mr.Flags & storedFlags}
	if r.gone() {
		return r
	}
	r.x = int32(core.QuantizePos(mr.X))
	r.y = int32(core.QuantizePos(mr.Y))
	r.o = core.QuantizeOrientation(mr.Orientation)
	if len(mr.Carriages) > 0 {
		r.car = make([]carriage, len(mr.Carriages))
		for k, c := range mr.Carriages {
			r.car[k] = carriage{x: int32(core.QuantizePos(c.X)), y: int32(core.QuantizePos(c.Y)), o: core.QuantizeOrientation(c.Orientation)}
		}
	}
	if r.flags&core.FlagRail != 0 {
		r.speed = core.QuantizeSpeed(mr.Speed)
		r.rstart = mr.RailStart
		if len(mr.Rail) > 0 {
			r.rail = append([]core.RailPiece(nil), mr.Rail...)
		}
	}
	return r
}

// sortByJourney sorts rows by (journey id, t, arrival).
func sortByJourney(rows []*row) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.meta.ID != b.meta.ID {
			return a.meta.ID < b.meta.ID
		}
		if a.t != b.t {
			return a.t < b.t
		}
		return a.seq < b.seq
	})
}

// dedupe keeps the last arrival of every (journey id, t) in rows sorted by sortByJourney.
func dedupe(rows []*row) []*row {
	out := rows[:0]
	for i, r := range rows {
		if i+1 < len(rows) && rows[i+1].meta.ID == r.meta.ID && rows[i+1].t == r.t {
			continue
		}
		out = append(out, r)
	}
	return out
}

// buildWindow makes a MoverWindow in the canonical order of core.MoverStore: one table
// entry per distinct MoverMeta, sorted by (ID, first row T), rows sorted by (entry, T).
// rows must not hold two rows with equal (journey id, t).
func buildWindow(sfc core.SurfaceKey, kind core.WindowKind, start, length core.TimeMs, rows []*row) *core.MoverWindow {
	type entry struct {
		meta  core.MoverMeta
		first core.TimeMs
		index int
	}
	entries := map[core.MoverMeta]*entry{}
	// byPtr caches the entry per meta pointer: rows share interned pointers, and
	// hashing a pointer is much cheaper than hashing the strings of a MoverMeta.
	byPtr := map[*core.MoverMeta]*entry{}
	lookup := func(p *core.MoverMeta) *entry {
		if e := byPtr[p]; e != nil {
			return e
		}
		e := entries[*p]
		if e == nil {
			e = &entry{meta: *p, first: math.MaxInt64}
			entries[*p] = e
		}
		byPtr[p] = e
		return e
	}
	for _, r := range rows {
		if e := lookup(r.meta); r.t < e.first {
			e.first = r.t
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
	w := &core.MoverWindow{Surface: sfc, Kind: kind, Start: start, Len: length, Movers: make([]core.MoverMeta, len(list))}
	for i, e := range list {
		w.Movers[i] = e.meta
		e.index = i
	}
	w.Rows = make([]core.MoverRow, 0, len(rows))
	for _, r := range rows {
		w.Rows = append(w.Rows, r.coreRow(lookup(r.meta).index))
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
