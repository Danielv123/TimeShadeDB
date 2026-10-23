// Window, keyframe and LiveMover shapes ported from illisis-minimap store/movers.ts and
// contracts.ts. Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"encoding/json"
	"fmt"
	"math"

	"timeshadedb/internal/core"
)

// Row flags of decoded source rows.
const (
	rowGone uint8 = 1 << iota
	rowHasH
	rowBoundary
	rowSeed
	rowEndSwitch
)

// srcRow is one decoded window row (or keyframe seed) of one surface.
type srcRow struct {
	t      int64
	x, y   float64
	vx, vy float64
	h      float64
	meta   *core.MoverMeta // interned per surface; never modified after interning
	flags  uint8
}

// windowJSON is a .m.gz window: {t, movers, s}.
type windowJSON struct {
	T      int64       `json:"t"`
	Movers [][]any     `json:"movers"`
	S      [][]float64 `json:"s"`
}

// keyframeJSON is a .k.gz keyframe: {t, movers: [LiveMover]}.
type keyframeJSON struct {
	T      int64   `json:"t"`
	Movers [][]any `json:"movers"`
}

const (
	maxCoordAbs = 1e7
	maxSpeedAbs = 1e4
)

func finite(v float64, lim float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= lim
}

func str(v any, max int) (string, bool) {
	s, ok := v.(string)
	return s, ok && len(s) <= max
}

func integer(v any, lo, hi float64) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < lo || f > hi {
		return 0, false
	}
	return int64(f), true
}

// parseMeta reads a window mover entry [id, kind, force, name] or, for trains,
// [id, "t", force, name, n, kinds, colour].
func parseMeta(m []any) (core.MoverMeta, bool) {
	if len(m) < 4 {
		return core.MoverMeta{}, false
	}
	var meta core.MoverMeta
	var ok bool
	var kind string
	if meta.ID, ok = str(m[0], core.MaxNameBytes); !ok || meta.ID == "" {
		return meta, false
	}
	if kind, ok = str(m[1], 1); !ok || !core.MoverKind(kind).Valid() {
		return meta, false
	}
	meta.Kind = core.MoverKind(kind)
	if meta.Force, ok = str(m[2], core.MaxNameBytes); !ok {
		return meta, false
	}
	if meta.Name, ok = str(m[3], core.MaxNameBytes); !ok {
		return meta, false
	}
	if meta.Kind == core.KindTrain && len(m) >= 7 {
		n, ok1 := integer(m[4], 0, 10000)
		kinds, ok2 := str(m[5], 1000)
		colour, ok3 := str(m[6], 16)
		if !ok1 || !ok2 || !ok3 {
			return meta, false
		}
		meta.Carriages, meta.Kinds, meta.Color = int(n), kinds, colour
	}
	return meta, true
}

// decodedWindow is one window file's rows before they are merged into the surface.
type decodedWindow struct {
	metas   []core.MoverMeta
	metaOK  []bool
	rows    []srcRow // meta is nil; mi holds the window-local mover index
	mi      []int32
	first   int64 // the file's time (its earliest row)
	last    int64
	invalid int
	err     error
}

func decodeWindowFile(path string) decodedWindow {
	var out decodedWindow
	b, err := readGz(path, maxWindowBytes)
	if err != nil {
		out.err = err
		return out
	}
	var w windowJSON
	if err := json.Unmarshal(b, &w); err != nil {
		out.err = fmt.Errorf("%s: %w: %v", path, ErrFormat, err)
		return out
	}
	if !saneTime(w.T) {
		out.err = fmt.Errorf("%s: %w: window time %d", path, ErrFormat, w.T)
		return out
	}
	out.metas = make([]core.MoverMeta, len(w.Movers))
	out.metaOK = make([]bool, len(w.Movers))
	for i, m := range w.Movers {
		out.metas[i], out.metaOK[i] = parseMeta(m)
	}
	out.rows = make([]srcRow, 0, len(w.S))
	out.mi = make([]int32, 0, len(w.S))
	out.first = w.T
	out.last = math.MinInt64
	for _, r := range w.S {
		if len(r) != 2 && len(r) < 6 {
			out.invalid++
			continue
		}
		idx, dt := r[0], r[1]
		if idx != math.Trunc(idx) || idx < 0 || int(idx) >= len(w.Movers) || !out.metaOK[int(idx)] ||
			dt != math.Trunc(dt) || dt < 0 || dt > float64(core.Day) {
			out.invalid++
			continue
		}
		row := srcRow{t: w.T + int64(dt)}
		if len(r) == 2 {
			row.flags = rowGone
		} else {
			row.x, row.y, row.vx, row.vy = r[2], r[3], r[4], r[5]
			if !finite(row.x, maxCoordAbs) || !finite(row.y, maxCoordAbs) || !finite(row.vx, maxSpeedAbs) || !finite(row.vy, maxSpeedAbs) {
				out.invalid++
				continue
			}
			if len(r) > 6 && finite(r[6], 2) {
				row.h = r[6]
				row.flags |= rowHasH
			}
		}
		out.rows = append(out.rows, row)
		out.mi = append(out.mi, int32(idx))
		out.last = max(out.last, row.t)
	}
	return out
}

// MoverCounts counts journeys, samples and gones by kind ("t", "v", "p").
type MoverCounts struct {
	Journeys map[string]int `json:"journeys"`
	Samples  map[string]int `json:"samples"`
	Gones    map[string]int `json:"gones"`
}

func newCounts() MoverCounts {
	return MoverCounts{Journeys: map[string]int{}, Samples: map[string]int{}, Gones: map[string]int{}}
}

// MoverReport counts what the mover import read, fixed and wrote for one surface.
type MoverReport struct {
	Surface       string `json:"surface"`
	WindowFiles   int    `json:"window_files"`
	KeyframeFiles int    `json:"keyframe_files"`
	// Source counts the rows as stored by illisis (research/out/movers.txt).
	Source MoverCounts `json:"source"`
	// SourceDupPairs counts consecutive samples of a journey with the same time and no
	// gone between them (movers.txt same_mover_same_t).
	SourceDupPairs int `json:"source_dup_pairs"`
	// InvalidRows were dropped while decoding (bad index, time or coordinates).
	InvalidRows int `json:"invalid_rows"`
	// BoundaryStamped samples sit on an illisis window floor; Retimed of them (by kind)
	// were moved earlier by the clamp fix.
	BoundaryStamped int            `json:"boundary_stamped"`
	Retimed         map[string]int `json:"retimed"`
	// RetimedGones are boundary-stamped gones moved back before a re-timed sample.
	RetimedGones int `json:"retimed_gones"`
	// EndSwitches are train samples flagged core.FlagEndSwitch: a low-speed jump as long
	// as the train ((carriages - 1) * 7 tiles), the leading end having flipped.
	EndSwitches int `json:"end_switches"`
	// DupSamplesDropped and DupGonesDropped are rows dropped because a later row of the
	// same journey had the same time. Re-timing never creates equal times, so these are
	// exactly the source rows followed by a row of the same journey with an equal stamp.
	DupSamplesDropped int `json:"dup_samples_dropped"`
	DupGonesDropped   int `json:"dup_gones_dropped"`
	// Keyframe rows read, rows whose sample time equals the journey's last window row
	// at or before the keyframe, seeds imported for journeys with no window row at or
	// before the keyframe, and conflicts (the keyframe lists a journey whose last window
	// row at or before it is a gone).
	KeyframeRows      int `json:"keyframe_rows"`
	KeyframeMatched   int `json:"keyframe_matched"`
	KeyframeSeeded    int `json:"keyframe_seeded"`
	KeyframeConflicts int `json:"keyframe_conflicts"`
	// KeyframeSeedsClamped seeds had a sample time more than seedMaxLag before their
	// keyframe and were placed seedMaxLag before it.
	KeyframeSeedsClamped int `json:"keyframe_seeds_clamped"`
	// OpenRunCuts counts the times rows of a journey were fixed before the row after
	// them was known, because no row followed within openRunLag (the streaming bound).
	OpenRunCuts int `json:"open_run_cuts"`
	// HeadingsDerived vehicle samples got their orientation from the velocity (they
	// still carry FlagNoHeading, since the orientation is an estimate); NoHeading
	// samples carry FlagNoHeading; Stops carry FlagStop.
	HeadingsDerived int `json:"headings_derived"`
	NoHeading       int `json:"no_heading"`
	Stops           int `json:"stops"`
	// Handoffs are imported gones followed by a sample of the same journey within
	// core.HandoffWindow (the store flags the same rows).
	Handoffs int `json:"handoffs"`
	// CrossingsInferred are gones the import added for Universal Edges crossings the
	// source has no gone for: a train sample behind the previous one, heading
	// back towards it, both near the same edge line (isCrossing). Each is a handoff.
	CrossingsInferred int `json:"crossings_inferred"`
	// TrainJumps counts consecutive train samples (0 < dt <= 3 s, no gone between)
	// that moved more than max speed * dt + 6 tiles, before and after the fixes
	// (research/scripts/jumps.py). After the fixes, end switches are not counted.
	TrainJumpsBefore int `json:"train_jumps_before"`
	TrainJumpsAfter  int `json:"train_jumps_after"`
	// Imported counts the samples handed to the store.
	Imported MoverCounts `json:"imported"`
	FirstT   core.TimeMs `json:"first"`
	LastT    core.TimeMs `json:"last"`
	// ErrorCount files failed to read or decode; Errors lists the first 20.
	ErrorCount int      `json:"error_count"`
	Errors     []string `json:"errors,omitempty"`
}

func (r *MoverReport) addErr(err error) {
	r.ErrorCount++
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, err.Error())
	}
}

// Clamp fix tuning.
const (
	// retimeMinSpeed: below this speed (tiles/s) the travel time is too uncertain to
	// re-time a sample.
	retimeMinSpeed = 0.5
	// retimeMinDist: below this distance (tiles) the sample did not move.
	retimeMinDist = 0.05
	// retimeMinShift: a sample is only re-timed when the estimate is this much earlier.
	retimeMinShift = 50
	// retimeMaxShift: clamped samples arrived at most a few seconds late (instances
	// forward batches every second); larger estimates are not trusted.
	retimeMaxShift = 5000
	// boundarySlack: a window file whose time is within this many ms after an illisis
	// window boundary starts at a floor, so its rows at that exact time are clamped.
	boundarySlack = 1000
	// carriagePitch is the distance between carriage centres in tiles (end switches).
	carriagePitch = 7
	// endSwitchMaxDt bounds the time between the two samples of an end switch (s), the
	// same window as trainJumps: a jump after a longer gap is a gap, not a flip.
	endSwitchMaxDt = 3
)

func dist(a, b *srcRow) float64 { return math.Hypot(a.x-b.x, a.y-b.y) }
func speed(r *srcRow) float64   { return math.Hypot(r.vx, r.vy) }

// retimeRows applies the clamp re-timing to rows (one journey, sorted by t). rows[:from]
// are context that was fixed (and emitted) earlier: they are read but never changed.
//
// Re-timing works on runs of consecutive boundary-stamped samples (no gone inside). The
// true times of a run lie before its stamps. With a good sample P before the run and a
// good sample N after it, and when the distance P..N over the time P..N agrees with
// the reported speeds within a factor of 2, the run is spread over P..N in proportion
// to the distance travelled. Otherwise each sample is placed at the previous one plus
// its distance over its reported speed (or, after a gone, at the next one minus that).
// No row moves later than its stamp, rows keep their order, and a re-timed row never
// lands on another row's time.
func retimeRows(rows []srcRow, from int, kind string, rep *MoverReport) {
	isRun := func(r *srcRow) bool { return r.flags&(rowGone|rowBoundary) == rowBoundary }
	for i := from; i < len(rows); {
		if !isRun(&rows[i]) {
			i++
			continue
		}
		j := i
		for j < len(rows) && isRun(&rows[j]) {
			j++
		}
		rep.BoundaryStamped += j - i
		retimeRun(rows, from, i, j, kind, rep)
		i = j
	}
}

// retimeRun re-times the boundary-stamped samples rows[i:j]; rows[:from] are frozen.
func retimeRun(rows []srcRow, from, i, j int, kind string, rep *MoverReport) {
	stamp := make([]int64, j-i)
	for k := i; k < j; k++ {
		stamp[k-i] = rows[k].t
	}
	hasP := i > 0 && rows[i-1].flags&rowGone == 0
	hasN := j < len(rows) && rows[j].flags&rowGone == 0 && rows[j].t > rows[j-1].t
	est := make([]int64, j-i)
	ok := false
	switch {
	case hasP && hasN:
		p, n := &rows[i-1], &rows[j]
		cum := make([]float64, j-i+1)
		total, vsum := 0.0, 0.0
		prev := p
		for k := i; k <= j; k++ {
			total += dist(prev, &rows[k])
			cum[k-i] = total
			vsum += speed(&rows[k])
			prev = &rows[k]
		}
		span := float64(n.t - p.t)
		vm := vsum / float64(j-i+1)
		if total > retimeMinDist && vm > retimeMinSpeed && span > 0 {
			va := total / (span / 1000)
			if va >= vm/2 && va <= vm*2 {
				for k := i; k < j; k++ {
					est[k-i] = p.t + int64(math.Round(span*cum[k-i]/total))
				}
				ok = true
			}
		}
		if !ok {
			ok = forwardEstimate(rows, i, j, est)
		}
	case hasP:
		ok = forwardEstimate(rows, i, j, est)
	case hasN:
		ok = backwardEstimate(rows, i, j, est)
		if ok && i > from && rows[i-1].flags&rowBoundary != 0 && est[0] <= rows[i-1].t && est[0] < stamp[0]-retimeMinShift {
			// A boundary-stamped gone right before the run was clamped too: it may move
			// back as far as the row before it allows.
			g := &rows[i-1]
			lb := int64(math.MinInt64)
			if i > 1 {
				lb = rows[i-2].t + 1
			}
			if nt := max(est[0]-1, lb); nt < g.t && nt >= g.t-retimeMaxShift {
				g.t = nt
				rep.RetimedGones++
			}
		}
	}
	if !ok {
		return
	}
	lb := int64(math.MinInt64)
	if i > 0 {
		lb = rows[i-1].t + 1
	}
	// A row that is not re-timed keeps its stamp, even when it equals the previous row's
	// time: equal times stay equal, so the duplicate rule keeps the later arrival. Only
	// a re-timed row is bounded by lb, so re-timing never reorders rows or creates a new
	// equal time.
	for k := i; k < j; k++ {
		t := min(max(est[k-i], lb), stamp[k-i])
		if t < stamp[k-i]-retimeMinShift && t >= stamp[k-i]-retimeMaxShift {
			rows[k].t = t
			rep.Retimed[kind]++
		} else {
			rows[k].t = stamp[k-i]
		}
		lb = rows[k].t + 1
	}
}

// forwardEstimate places each sample at the previous one plus distance over speed.
func forwardEstimate(rows []srcRow, i, j int, est []int64) bool {
	prevT := rows[i-1].t
	prev := &rows[i-1]
	moved := false
	for k := i; k < j; k++ {
		r := &rows[k]
		if d, v := dist(prev, r), speed(r); v > retimeMinSpeed && d > retimeMinDist {
			prevT += int64(math.Round(d / v * 1000))
			moved = true
		} else {
			prevT++
		}
		est[k-i] = prevT
		prev = r
	}
	return moved
}

// backwardEstimate places each sample at the next one minus distance over its speed.
func backwardEstimate(rows []srcRow, i, j int, est []int64) bool {
	nextT := rows[j].t
	next := &rows[j]
	moved := false
	for k := j - 1; k >= i; k-- {
		r := &rows[k]
		if d, v := dist(r, next), speed(next); v > retimeMinSpeed && d > retimeMinDist {
			nextT -= int64(math.Round(d / v * 1000))
			moved = true
		} else {
			nextT--
		}
		est[k-i] = nextT
		next = r
	}
	return moved
}

// parseLiveMover reads [id, kind, force, name, n, x, y, vx, vy, ts, kinds, colour, h].
func parseLiveMover(lm []any) (srcRow, core.MoverMeta, bool) {
	if len(lm) < 13 {
		return srcRow{}, core.MoverMeta{}, false
	}
	meta, ok := parseMeta([]any{lm[0], lm[1], lm[2], lm[3], lm[4], lm[10], lm[11]})
	if !ok {
		return srcRow{}, meta, false
	}
	if meta.Kind != core.KindTrain {
		meta.Carriages, meta.Kinds, meta.Color = 0, "", ""
	}
	var r srcRow
	nums := [5]float64{}
	for i, j := range [5]int{5, 6, 7, 8, 12} {
		f, ok := lm[j].(float64)
		if !ok {
			return r, meta, false
		}
		nums[i] = f
	}
	ts, ok := integer(lm[9], 0, math.MaxInt64)
	if !ok || !saneTime(ts) {
		return r, meta, false
	}
	r = srcRow{t: ts, x: nums[0], y: nums[1], vx: nums[2], vy: nums[3]}
	if !finite(r.x, maxCoordAbs) || !finite(r.y, maxCoordAbs) || !finite(r.vx, maxSpeedAbs) || !finite(r.vy, maxSpeedAbs) {
		return r, meta, false
	}
	if meta.Kind == core.KindTrain && finite(nums[4], 2) {
		r.h, r.flags = nums[4], rowHasH
	}
	return r, meta, true
}

// headingFromVelocity converts a velocity to a Factorio orientation (0 north, 0.25 east).
func headingFromVelocity(vx, vy float64) float64 {
	o := math.Atan2(vx, -vy) / (2 * math.Pi)
	if o < 0 {
		o++
	}
	return o
}
