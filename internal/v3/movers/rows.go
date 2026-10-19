package movers

import (
	"fmt"
	"math"
	"sort"

	"timeshadedb/internal/v3/core"
)

// storedFlags are the core.MoverFlags bits a store keeps. Bits 6 and 7 are wire-only or
// reserved (docs/v3/FORMAT.md "MVW3"), so they are cleared on Append.
const storedFlags = core.FlagStop | core.FlagGone | core.FlagHandoff | core.FlagEndSwitch | core.FlagNoTick | core.FlagNoHeading

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
	car   []carriage
	// seq is the arrival order; for equal (journey, t) the highest seq wins.
	seq uint64
}

// rowBytes is the approximate heap cost of one row without carriages, for budgets.
const rowBytes = 72

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
// coordinates outside the representable range. FlagHandoff is cleared (stores assign
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
