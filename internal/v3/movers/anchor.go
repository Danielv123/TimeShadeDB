package movers

import (
	"container/list"
	"math"

	"timeshadedb/internal/v3/core"
)

// Clock anchors (docs/v3/PROTOCOL.md 4, PLAN.md §4.1) map a source's game ticks to the
// map clock. Mover samples are stamped through an anchor and keep the raw tick, so
// velocity is exact even below 60 UPS, arrival jitter does not move samples, and two
// samples of one journey never collide on the same millisecond unless their ticks do.
//
// Every envelope gives a point (tick, recv_ms). Arrival lag is never negative, so the
// true clock is a line on or under every point. The anchor fits that line: its slope is
// the effective milliseconds per tick, measured, so a server running below 60*speed UPS
// (a busy map) does not drift away; its intercept is the PROTOCOL.md minimum taken at
// that slope:
//
//	nominal    = 1000 / (60 * (speed or 1))                     ms per tick
//	msPerTick  = least squares through the lowest-latency point of each of
//	             anchorSlices equal tick ranges of the last AnchorRateWindow, once
//	             it spans AnchorFitSpan of receive time and 4 points, if within
//	             [AnchorMinRate, AnchorMaxRate] * nominal; otherwise nominal
//	offset     = min(recv_ms - (tick - base) * msPerTick) over the last AnchorWindow
//	t_ms(tick) = round(offset + (tick - base) * msPerTick)
//
// If the slope over the last AnchorWindow differs from the long one by more than
// AnchorRateChange, the UPS changed and older points are dropped; if every point of the
// last AnchorDriftSpan is more than AnchorDrift above the line, the server slowed down
// faster than that and only the last 2*AnchorDriftSpan are kept. base is 0 until the
// game speed changes; then the anchor re-bases at the current tick and continues the
// clock where it is. The window resets, and the line re-fits from new observations,
// when an envelope's distance above the line changes by more than AnchorJump from the
// previous envelope's (save reload, pause, host clock step), when it is more than
// AnchorLost off the line, or when the tick goes backwards (TestAnchorSlowServer,
// TestAnchorRateChange measure the errors).

// Anchor tuning (PROTOCOL.md 4).
const (
	// AnchorWindow is how long an observation can set the offset (the minimum).
	AnchorWindow core.TimeMs = 60 * core.Second
	// AnchorRateWindow is how long an observation stays in the rate fit; a longer
	// baseline than AnchorWindow keeps arrival jitter from tilting the slope.
	AnchorRateWindow core.TimeMs = 3 * core.Minute
	// AnchorRateChange is the relative difference between the rate fitted over the
	// last AnchorWindow and over AnchorRateWindow beyond which the rate is considered
	// changed and the older observations are dropped.
	AnchorRateChange = 0.01
	// AnchorDrift is the lowest lag above the line, over the last AnchorDriftSpan, that
	// means the source slowed down faster than the fit follows.
	AnchorDrift = 500.0
	// AnchorDriftSpan is the receive-time span AnchorDrift is measured over.
	AnchorDriftSpan core.TimeMs = 5 * core.Second
	// AnchorJump is the change of an envelope's distance from the line, against the
	// previous envelope, that resets the anchor.
	AnchorJump = 5000.0
	// AnchorLost is the distance from the line that resets the anchor however slowly
	// it built up (a rate outside the fitted range).
	AnchorLost = 30000.0
	// AnchorFitSpan is the receive-time span the window needs before the rate is fitted.
	AnchorFitSpan core.TimeMs = 5 * core.Second
	// AnchorMinRate and AnchorMaxRate bound the fitted ms per tick relative to the
	// nominal: a game cannot run faster than 60*speed UPS (some slack for jitter), and
	// below 1/30 of it (2 UPS at speed 1) the fit is not trusted.
	AnchorMinRate = 0.95
	AnchorMaxRate = 30.0
	// anchorMaxPoints bounds the window; faster envelopes keep only the newest points.
	anchorMaxPoints = 1024
	// anchorSlices is the number of tick ranges whose lowest-latency observations the
	// slope is fitted to.
	anchorSlices = 12
)

// anchorPt is one observation: x ticks after base, received at map time y.
type anchorPt struct {
	x, y float64
}

// Anchor is the clock anchor of one source. The zero value is ready to use.
type Anchor struct {
	started   bool
	tps       float64 // nominal ticks per second
	base      int64
	lastTick  int64
	lastLag   float64 // the previous envelope's distance above the line
	offset    float64 // map time of tick base
	msPerTick float64
	pts       []anchorPt // x increasing, one point per tick (the lowest y)
	picks     []anchorPt // scratch for fit
	resets    int
}

func tpsOf(speed float64) float64 {
	if !(speed > 0) || math.IsInf(speed, 0) {
		speed = 1
	}
	return 60 * speed
}

// Observe adds one envelope: the forwarder's receive time, the envelope tick and the
// game speed (0 means 1).
func (a *Anchor) Observe(recv core.TimeMs, tick int64, speed float64) {
	tps := tpsOf(speed)
	y := float64(recv)
	switch {
	case !a.started:
		a.started, a.tps, a.base = true, tps, 0
		a.restart(y, tick)
		return
	case tick < a.lastTick:
		// A new game or a reloaded save: start over.
		a.tps, a.base = tps, 0
		a.resets++
		a.restart(y, tick)
		return
	case tps != a.tps:
		// Re-base at the current tick and continue the clock where it is: the window
		// restarts from a virtual observation on the old line.
		cont := a.offset + float64(tick-a.base)*a.msPerTick
		a.base, a.tps, a.offset, a.msPerTick = tick, tps, cont, 1000/tps
		a.pts = append(a.pts[:0], anchorPt{0, cont})
		a.resets++
	}
	// A jump: the lag changed by more than AnchorJump since the previous envelope
	// (pause, save reload, host clock step), or the line lost track of the source.
	x := float64(tick - a.base)
	lag := y - (a.offset + x*a.msPerTick)
	if math.Abs(lag-a.lastLag) > AnchorJump || math.Abs(lag) > AnchorLost {
		a.resets++
		a.restart(y, tick)
		return
	}
	a.lastTick, a.lastLag = tick, lag
	if n := len(a.pts); n > 0 && a.pts[n-1].x == x {
		a.pts[n-1].y = math.Min(a.pts[n-1].y, y) // the same tick again
	} else {
		a.pts = append(a.pts, anchorPt{x, y})
	}
	// Expire old points (the newest always stays).
	drop := 0
	for len(a.pts)-drop > 1 && a.pts[drop].y < y-float64(AnchorRateWindow) {
		drop++
	}
	if len(a.pts)-drop > anchorMaxPoints {
		drop = len(a.pts) - anchorMaxPoints
	}
	if drop > 0 {
		a.pts = append(a.pts[:0], a.pts[drop:]...)
	}
	a.fit()
	a.lastLag = y - (a.offset + x*a.msPerTick) // against the new line
}

// restart empties the window and fits the line to this envelope alone, at the nominal
// rate.
func (a *Anchor) restart(y float64, tick int64) {
	a.msPerTick = 1000 / a.tps
	x := float64(tick - a.base)
	a.pts = append(a.pts[:0], anchorPt{x, y})
	a.offset, a.lastTick, a.lastLag = y-x*a.msPerTick, tick, 0
}

// fit sets offset and msPerTick from the window.
func (a *Anchor) fit() {
	nominal := 1000 / a.tps
	last := a.pts[len(a.pts)-1]
	recent := 0 // the first point of the last AnchorWindow
	for recent < len(a.pts)-1 && a.pts[recent].y < last.y-float64(AnchorWindow) {
		recent++
	}
	slope, ok := a.slopeOver(a.pts, nominal)
	if recent > 0 {
		// The long fit spans AnchorRateWindow. If the last AnchorWindow alone has a
		// clearly different rate, the rate changed: forget the older points.
		if s, ok2 := a.slopeOver(a.pts[recent:], nominal); ok2 && (!ok || math.Abs(s/slope-1) > AnchorRateChange) {
			a.pts = append(a.pts[:0], a.pts[recent:]...)
			recent, slope, ok = 0, s, true
		}
	}
	if !ok {
		// Too little data or an implausible slope: the nominal rate (the PROTOCOL.md
		// minimum).
		slope = nominal
	}
	a.setLine(slope, recent)
	// A server that slowed down abruptly leaves every recent envelope well above the
	// line until the windows have moved past the change: start the fit over from the
	// last 2*AnchorDriftSpan.
	// The spans are measured in ticks on the line, so one late envelope cannot narrow
	// them, and need at least 3 envelopes.
	driftTicks := float64(AnchorDriftSpan) / a.msPerTick
	if len(a.pts) > 3 && a.pts[0].x < last.x-2*driftTicks {
		minLag, n := math.Inf(1), 0
		for i := len(a.pts) - 1; i >= 0 && a.pts[i].x >= last.x-driftTicks; i-- {
			minLag = math.Min(minLag, a.pts[i].y-(a.offset+a.pts[i].x*a.msPerTick))
			n++
		}
		if n >= 3 && minLag > AnchorDrift {
			keep := len(a.pts) - 1
			for keep > 0 && a.pts[keep-1].x >= last.x-2*driftTicks {
				keep--
			}
			a.pts = append(a.pts[:0], a.pts[keep:]...)
			if s, ok := a.slopeOver(a.pts, nominal); ok {
				slope = s
			}
			a.setLine(slope, 0)
		}
	}
}

// setLine sets msPerTick to slope and the offset so the line passes through the lowest
// observation of pts[from:] (the last AnchorWindow) at that slope; coordinates are
// relative to the newest point so the arithmetic stays exact.
func (a *Anchor) setLine(slope float64, from int) {
	last := a.pts[len(a.pts)-1]
	low := math.Inf(1)
	for _, p := range a.pts[from:] {
		low = math.Min(low, (p.y-last.y)-(p.x-last.x)*slope)
	}
	a.msPerTick = slope
	a.offset = last.y + low - last.x*slope
}

// slopeOver fits the rate to pts: least squares through the lowest-latency observation
// of each of anchorSlices equal tick ranges ("lowest" judged against the current
// slope). It reports false with fewer than 3 points, less than AnchorFitSpan of receive
// time, or a slope outside [AnchorMinRate, AnchorMaxRate] * nominal. Coordinates are
// relative to the first point, so the sums stay exact for large ticks and epoch ms.
func (a *Anchor) slopeOver(pts []anchorPt, nominal float64) (float64, bool) {
	first, last := pts[0], pts[len(pts)-1]
	if len(pts) < 4 || last.y-first.y < float64(AnchorFitSpan) || last.x <= first.x {
		return 0, false
	}
	cur := a.msPerTick
	width := (last.x - first.x) / anchorSlices * (1 + 1e-9)
	picks := a.picks[:0]
	slice := -1
	for _, p := range pts {
		q := anchorPt{p.x - first.x, p.y - first.y}
		k := int(q.x / width)
		switch {
		case k != slice:
			slice = k
			picks = append(picks, q)
		case q.y-q.x*cur < picks[len(picks)-1].y-picks[len(picks)-1].x*cur:
			picks[len(picks)-1] = q
		}
	}
	a.picks = picks
	var mx, my float64
	for _, q := range picks {
		mx += q.x
		my += q.y
	}
	mx /= float64(len(picks))
	my /= float64(len(picks))
	var sxy, sxx float64
	for _, q := range picks {
		sxy += (q.x - mx) * (q.y - my)
		sxx += (q.x - mx) * (q.x - mx)
	}
	if len(picks) < 2 || sxx <= 0 {
		return 0, false
	}
	slope := sxy / sxx
	return slope, slope >= AnchorMinRate*nominal && slope <= AnchorMaxRate*nominal
}

// T returns the map-clock time of tick. Before the first Observe it returns 0.
func (a *Anchor) T(tick int64) core.TimeMs {
	if !a.started {
		return 0
	}
	return core.TimeMs(math.Round(a.offset + float64(tick-a.base)*a.msPerTick))
}

// Offset returns the current offset in ms (the map-clock time of tick base), for the
// tsdb_clock_anchor_offset_ms metric.
func (a *Anchor) Offset() float64 { return a.offset }

// UPS returns the effective updates per second the anchor uses: the fitted rate, or
// 60*speed while it is not fitted. 0 before the first Observe.
func (a *Anchor) UPS() float64 {
	if !a.started {
		return 0
	}
	return 1000 / a.msPerTick
}

// Resets counts how often the anchor re-fitted after a jump, a backwards tick or a speed
// change.
func (a *Anchor) Resets() int { return a.resets }

// Anchors keeps one Anchor per source id (envelope "src"), bounded: beyond max sources
// the least recently used is forgotten (it re-fits from its next envelope).
type Anchors struct {
	max  int
	byID map[string]*list.Element
	lru  *list.List
}

type namedAnchor struct {
	src string
	Anchor
}

// NewAnchors returns a set of at most max anchors (default 4096 when max <= 0).
func NewAnchors(max int) *Anchors {
	if max <= 0 {
		max = 4096
	}
	return &Anchors{max: max, byID: map[string]*list.Element{}, lru: list.New()}
}

// Observe adds an envelope of src and returns its anchor, valid until the next call.
func (as *Anchors) Observe(src string, recv core.TimeMs, tick int64, speed float64) *Anchor {
	e := as.byID[src]
	if e == nil {
		e = as.lru.PushFront(&namedAnchor{src: src})
		as.byID[src] = e
		for as.lru.Len() > as.max {
			old := as.lru.Back()
			as.lru.Remove(old)
			delete(as.byID, old.Value.(*namedAnchor).src)
		}
	} else {
		as.lru.MoveToFront(e)
	}
	a := &e.Value.(*namedAnchor).Anchor
	a.Observe(recv, tick, speed)
	return a
}

// Get returns the anchor of src, or nil.
func (as *Anchors) Get(src string) *Anchor {
	if e := as.byID[src]; e != nil {
		return &e.Value.(*namedAnchor).Anchor
	}
	return nil
}

// Offsets calls fn for every source with its offset (for metrics).
func (as *Anchors) Offsets(fn func(src string, offset float64)) {
	for e := as.lru.Front(); e != nil; e = e.Next() {
		n := e.Value.(*namedAnchor)
		fn(n.src, n.offset)
	}
}
