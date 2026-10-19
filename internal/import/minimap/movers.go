// Window, keyframe and LiveMover shapes ported from illisis-minimap store/movers.ts and
// contracts.ts. Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
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
	meta   int32 // index into the surface's meta table
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
	rows    []srcRow // meta holds the window-local mover index
	first   int64    // the file's time (its earliest row)
	last    int64
	invalid int
	err     error
}

func decodeWindowFile(path string) decodedWindow {
	var out decodedWindow
	b, err := readGz(path, maxJSONBytes)
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
		row := srcRow{t: w.T + int64(dt), meta: int32(idx)}
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
	// same journey had the same time (after re-timing).
	DupSamplesDropped int `json:"dup_samples_dropped"`
	DupGonesDropped   int `json:"dup_gones_dropped"`
	// Keyframe rows read, rows matching a window row (same journey and time), seeds
	// imported for journeys with no row at or before the keyframe, and conflicts (the
	// keyframe lists a journey whose last row before it is a gone).
	KeyframeRows      int `json:"keyframe_rows"`
	KeyframeMatched   int `json:"keyframe_matched"`
	KeyframeSeeded    int `json:"keyframe_seeded"`
	KeyframeConflicts int `json:"keyframe_conflicts"`
	// HeadingsDerived vehicle samples got their orientation from the velocity;
	// NoHeading samples carry FlagNoHeading; Stops carry FlagStop.
	HeadingsDerived int `json:"headings_derived"`
	NoHeading       int `json:"no_heading"`
	Stops           int `json:"stops"`
	// Handoffs are imported gones followed by a sample of the same journey within
	// core.HandoffWindow (the store flags the same rows).
	Handoffs int `json:"handoffs"`
	// TrainJumps counts consecutive train samples (0 < dt <= 3 s, no gone between)
	// that moved more than max speed * dt + 6 tiles, before and after the fixes
	// (research/scripts/jumps.py). After the fixes, end switches are not counted.
	TrainJumpsBefore int `json:"train_jumps_before"`
	TrainJumpsAfter  int `json:"train_jumps_after"`
	// Imported counts the samples handed to the store.
	Imported MoverCounts `json:"imported"`
	FirstT   core.TimeMs `json:"first"`
	LastT    core.TimeMs `json:"last"`
	Errors   []string    `json:"errors,omitempty"`
}

func (r *MoverReport) addErr(err error) {
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, err.Error())
	}
}

// surfaceData is one surface after decoding and fixing.
type surfaceData struct {
	key      core.SurfaceKey
	metas    []core.MoverMeta
	journeys []string
	rows     map[string][]srcRow // per journey, sorted by t, fixed
	rep      *MoverReport
}

// loadSurface decodes, fixes and seeds one surface's movers.
func loadSurface(ctx context.Context, d *Dataset, src *SourceSurface, workers int) (*surfaceData, error) {
	rep := &MoverReport{Surface: src.Key.ID(), WindowFiles: len(src.Windows), KeyframeFiles: len(src.Keyframes),
		Source: newCounts(), Imported: newCounts(), Retimed: map[string]int{}}
	sd := &surfaceData{key: src.Key, rows: map[string][]srcRow{}, rep: rep}

	// Decode windows in parallel, merge in file order.
	decoded := make([]decodedWindow, len(src.Windows))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				decoded[j] = decodeWindowFile(src.WindowPath(src.Windows[j]))
			}
		}()
	}
	for j := range src.Windows {
		if ctx.Err() != nil {
			break
		}
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	metaIdx := map[core.MoverMeta]int32{}
	intern := func(m core.MoverMeta) int32 {
		i, ok := metaIdx[m]
		if !ok {
			i = int32(len(sd.metas))
			sd.metas = append(sd.metas, m)
			metaIdx[m] = i
		}
		return i
	}
	W := d.WindowMs
	for j := range decoded {
		dw := &decoded[j]
		if dw.err != nil {
			rep.addErr(dw.err)
			continue
		}
		rep.InvalidRows += dw.invalid
		local := make([]int32, len(dw.metas))
		for i, m := range dw.metas {
			if dw.metaOK[i] {
				local[i] = intern(m)
			}
		}
		for _, r := range dw.rows {
			r.meta = local[r.meta]
			// The floor after a close is the close time + 1 (or later), and a clamped
			// sample is stamped with it, so it is the earliest time in its file.
			if mod := r.t % W; mod <= 2 || (r.t == dw.first && dw.first%W < boundarySlack) {
				r.flags |= rowBoundary
			}
			id := sd.metas[r.meta].ID
			sd.rows[id] = append(sd.rows[id], r)
		}
		decoded[j] = decodedWindow{}
	}
	for id := range sd.rows {
		sd.journeys = append(sd.journeys, id)
	}
	sort.Strings(sd.journeys)

	for _, id := range sd.journeys {
		rows := sd.rows[id]
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].t < rows[b].t })
		kind := string(sd.metas[rows[0].meta].Kind)
		rep.Source.Journeys[kind]++
		for _, r := range rows {
			k := string(sd.metas[r.meta].Kind)
			if r.flags&rowGone != 0 {
				rep.Source.Gones[k]++
			} else {
				rep.Source.Samples[k]++
			}
		}
		rep.SourceDupPairs += dupPairs(rows)
		if kind == string(core.KindTrain) {
			rep.TrainJumpsBefore += trainJumps(rows)
		}
		rows = fixJourney(rows, sd.metas, rep)
		if kind == string(core.KindTrain) {
			rep.TrainJumpsAfter += trainJumps(rows)
		}
		sd.rows[id] = rows
	}

	if err := seedKeyframes(ctx, src, sd, intern); err != nil {
		return nil, err
	}

	for _, id := range sd.journeys {
		rows := sd.rows[id]
		if len(rows) == 0 {
			continue
		}
		rep.Imported.Journeys[string(sd.metas[rows[0].meta].Kind)]++
		for i, r := range rows {
			k := string(sd.metas[r.meta].Kind)
			if r.flags&rowGone != 0 {
				rep.Imported.Gones[k]++
				for _, n := range rows[i+1:] {
					if n.t > r.t+int64(core.HandoffWindow) {
						break
					}
					if n.flags&rowGone == 0 {
						rep.Handoffs++
						break
					}
				}
			} else {
				rep.Imported.Samples[k]++
			}
			if rep.FirstT == 0 || core.TimeMs(r.t) < rep.FirstT {
				rep.FirstT = core.TimeMs(r.t)
			}
			rep.LastT = max(rep.LastT, core.TimeMs(r.t))
		}
	}
	return sd, nil
}

// dupPairs counts consecutive samples with equal times, a gone resetting the chain.
func dupPairs(rows []srcRow) int {
	n := 0
	prev := -1
	for i, r := range rows {
		if r.flags&rowGone != 0 {
			prev = -1
			continue
		}
		if prev >= 0 && rows[prev].t == r.t {
			n++
		}
		prev = i
	}
	return n
}

// trainJumps is research/scripts/jumps.py: consecutive samples (no gone between) with
// 0 < dt <= 3 s whose displacement exceeds max(|v|, |v_prev|) * dt + 6 tiles.
func trainJumps(rows []srcRow) int {
	n := 0
	prev := -1
	for i, r := range rows {
		if r.flags&rowGone != 0 {
			prev = -1
			continue
		}
		if prev >= 0 && r.flags&rowEndSwitch == 0 {
			p := rows[prev]
			dt := float64(r.t-p.t) / 1000
			if dt > 0 && dt <= 3 {
				v := max(math.Hypot(r.vx, r.vy), math.Hypot(p.vx, p.vy))
				if math.Hypot(r.x-p.x, r.y-p.y) > v*dt+6 {
					n++
				}
			}
		}
		prev = i
	}
	return n
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

// fixJourney applies the clamp re-timing, the duplicate rule and end-switch detection to
// one journey's rows (sorted by t). It returns the fixed rows, still sorted by t.
//
// Re-timing works on runs of consecutive boundary-stamped samples (no gone inside). The
// true times of a run lie before its stamps. With a good sample P before the run and a
// good sample N after it, and when the distance P..N over the time P..N agrees with
// the reported speeds within a factor of 2, the run is spread over P..N in proportion
// to the distance travelled. Otherwise each sample is placed at the previous one plus
// its distance over its reported speed (or, after a gone, at the next one minus that).
// No sample moves later than its stamp, and order is kept.
func fixJourney(rows []srcRow, metas []core.MoverMeta, rep *MoverReport) []srcRow {
	kind := string(metas[rows[0].meta].Kind)
	isRun := func(r *srcRow) bool { return r.flags&(rowGone|rowBoundary) == rowBoundary }
	for i := 0; i < len(rows); {
		if !isRun(&rows[i]) {
			i++
			continue
		}
		j := i
		for j < len(rows) && isRun(&rows[j]) {
			j++
		}
		rep.BoundaryStamped += j - i
		retimeRun(rows, i, j, kind, rep)
		i = j
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].t < rows[b].t })
	out := rows[:0]
	for i := range rows {
		if i+1 < len(rows) && rows[i+1].t == rows[i].t {
			if rows[i].flags&rowGone != 0 {
				rep.DupGonesDropped++
			} else {
				rep.DupSamplesDropped++
			}
			continue
		}
		out = append(out, rows[i])
	}
	if kind == string(core.KindTrain) {
		markEndSwitches(out, metas, rep)
	}
	return out
}

// retimeRun re-times the boundary-stamped samples rows[i:j].
func retimeRun(rows []srcRow, i, j int, kind string, rep *MoverReport) {
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
		if ok && i > 0 && rows[i-1].flags&rowBoundary != 0 && est[0] <= rows[i-1].t && est[0] < stamp[0]-retimeMinShift {
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

// markEndSwitches flags train samples where the leading end flipped: a jump (as in
// trainJumps, 0 < dt <= 3 s) at low speed whose length matches the train's front-to-back
// distance.
func markEndSwitches(rows []srcRow, metas []core.MoverMeta, rep *MoverReport) {
	prev := -1
	for i := range rows {
		r := &rows[i]
		if r.flags&rowGone != 0 {
			prev = -1
			continue
		}
		if prev >= 0 {
			p := &rows[prev]
			n := metas[r.meta].Carriages
			dt := float64(r.t-p.t) / 1000
			d := dist(p, r)
			if n >= 2 && dt > 0 && dt <= endSwitchMaxDt && d > max(speed(r), speed(p))*dt+6 && min(speed(r), speed(p)) < 3 {
				l := float64((n - 1) * carriagePitch)
				if math.Abs(d-l) <= 0.3*l+2 {
					r.flags |= rowEndSwitch
					rep.EndSwitches++
				}
			}
		}
		prev = i
	}
}

// seedKeyframes reads the surface's keyframes in time order. A keyframe mover whose
// journey has no row at or before the keyframe is imported as a sample at its own time.
func seedKeyframes(ctx context.Context, src *SourceSurface, sd *surfaceData, intern func(core.MoverMeta) int32) error {
	rep := sd.rep
	for _, kt := range src.Keyframes {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := readGz(src.KeyframePath(kt), maxJSONBytes)
		if err != nil {
			rep.addErr(err)
			continue
		}
		var kf keyframeJSON
		if err := json.Unmarshal(b, &kf); err != nil {
			rep.addErr(fmt.Errorf("%s: %w: %v", src.KeyframePath(kt), ErrFormat, err))
			continue
		}
		for _, lm := range kf.Movers {
			rep.KeyframeRows++
			row, meta, ok := parseLiveMover(lm)
			if !ok || row.t > kf.T {
				rep.InvalidRows++
				continue
			}
			rows := sd.rows[meta.ID]
			i := sort.Search(len(rows), func(i int) bool { return rows[i].t > kf.T })
			if i > 0 {
				j := sort.Search(len(rows), func(j int) bool { return rows[j].t >= row.t })
				if j < len(rows) && rows[j].t == row.t {
					rep.KeyframeMatched++
				}
				if rows[i-1].flags&rowGone != 0 {
					rep.KeyframeConflicts++
				}
				continue
			}
			row.meta = intern(meta)
			row.flags |= rowSeed
			k := sort.Search(len(rows), func(k int) bool { return rows[k].t >= row.t })
			if k < len(rows) && rows[k].t == row.t {
				continue
			}
			rows = append(rows, srcRow{})
			copy(rows[k+1:], rows[k:])
			rows[k] = row
			if len(sd.rows[meta.ID]) == 0 {
				sd.journeys = append(sd.journeys, meta.ID)
			}
			sd.rows[meta.ID] = rows
			rep.KeyframeSeeded++
		}
	}
	sort.Strings(sd.journeys)
	return nil
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

// outSample is a fixed sample ready for the store, in compact form.
type outSample struct {
	t     int64
	x, y  float64
	o     float64
	flags core.MoverFlags
	meta  int32 // global meta index
	sfc   int32
}

// buildSamples converts a surface's fixed rows to outSamples, deriving flags and
// headings, and appends them to out.
func buildSamples(sd *surfaceData, sfc int32, metaBase int32, out []outSample) []outSample {
	rep := sd.rep
	for _, id := range sd.journeys {
		haveH := false
		lastH := 0.0
		for _, r := range sd.rows[id] {
			s := outSample{t: r.t, meta: metaBase + r.meta, sfc: sfc, flags: core.FlagNoTick}
			if r.flags&rowGone != 0 {
				s.flags |= core.FlagGone
				out = append(out, s)
				continue
			}
			s.x, s.y = r.x, r.y
			if r.flags&rowEndSwitch != 0 {
				s.flags |= core.FlagEndSwitch
			}
			if r.vx == 0 && r.vy == 0 {
				s.flags |= core.FlagStop
				rep.Stops++
			}
			switch {
			case r.flags&rowHasH != 0:
				s.o = r.h
			case sd.metas[r.meta].Kind == core.KindVehicle && (r.vx != 0 || r.vy != 0):
				lastH, haveH = headingFromVelocity(r.vx, r.vy), true
				s.o = lastH
				rep.HeadingsDerived++
			case sd.metas[r.meta].Kind == core.KindVehicle && haveH:
				s.o = lastH
				rep.HeadingsDerived++
			default:
				s.flags |= core.FlagNoHeading
				rep.NoHeading++
			}
			out = append(out, s)
		}
	}
	return out
}

// importMovers decodes and fixes every surface's movers and feeds them to the store in
// global time order. Each Commit trails the feed by more than core.HandoffWindow, so the
// store sees the sample that makes a gone a handoff before it closes the gone's window.
func importMovers(ctx context.Context, d *Dataset, opt *Options, store core.MoverStore) ([]*MoverReport, core.ApplyResult, error) {
	var res core.ApplyResult
	var surfaces []*surfaceData
	for _, src := range d.Surfaces {
		if !opt.wantSurface(src.Key) {
			continue
		}
		sd, err := loadSurface(ctx, d, src, opt.Workers)
		if err != nil {
			return nil, res, err
		}
		surfaces = append(surfaces, sd)
	}
	var (
		metas   []core.MoverMeta
		keys    []core.SurfaceKey
		samples []outSample
		reports []*MoverReport
	)
	for i, sd := range surfaces {
		samples = buildSamples(sd, int32(i), int32(len(metas)), samples)
		metas = append(metas, sd.metas...)
		keys = append(keys, sd.key)
		reports = append(reports, sd.rep)
		sd.rows = nil
	}
	sort.SliceStable(samples, func(a, b int) bool { return samples[a].t < samples[b].t })

	batch := make([]core.MoverSample, 0, 8192)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		r, err := store.Append(ctx, batch)
		res.Add(r)
		batch = batch[:0]
		return err
	}
	if len(samples) == 0 {
		return reports, res, nil
	}
	every := opt.MoverCommitEvery
	next := core.WindowStart(core.TimeMs(samples[0].t), core.MoverWindowLen) + every
	for _, s := range samples {
		t := core.TimeMs(s.t)
		for t > next+core.HandoffWindow {
			if err := flush(); err != nil {
				return reports, res, err
			}
			if err := store.Commit(ctx, next); err != nil {
				return reports, res, err
			}
			next += every
			if t > next+core.HandoffWindow+every {
				next = core.WindowStart(t-core.HandoffWindow, core.MoverWindowLen)
			}
		}
		batch = append(batch, core.MoverSample{
			Surface:     keys[s.sfc],
			Meta:        metas[s.meta],
			T:           t,
			X:           s.x,
			Y:           s.y,
			Orientation: s.o,
			Flags:       s.flags,
		})
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return reports, res, err
			}
		}
	}
	if err := flush(); err != nil {
		return reports, res, err
	}
	last := core.TimeMs(samples[len(samples)-1].t)
	if err := store.Commit(ctx, core.WindowEnd(last, core.MoverWindowLen)); err != nil {
		return reports, res, err
	}
	return reports, res, nil
}
