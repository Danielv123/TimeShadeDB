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
//	tps        = 60 * (speed or 1)
//	obs        = recv_ms - (tick - base) * 1000 / tps        (one value per envelope)
//	offset     = min(obs) over the last 60 s of envelopes     (the lowest-latency one)
//	t_ms(tick) = round(offset + (tick - base) * 1000 / tps)
//
// base is 0 until the game speed changes; then the anchor re-bases at the current tick
// so a speed change does not scale the whole tick count (with base 0 this is exactly
// the PROTOCOL.md formula). The window resets, and the offset re-fits from new
// observations, when obs moves more than AnchorJump away from the offset (save reload,
// pause, host clock step) or when the tick goes backwards.

// Anchor tuning (PROTOCOL.md 4).
const (
	// AnchorWindow is how long an observation can stay the minimum.
	AnchorWindow core.TimeMs = 60 * core.Second
	// AnchorJump is the observation jump that resets the anchor.
	AnchorJump = 5000.0
)

type anchorObs struct {
	recv core.TimeMs
	obs  float64
}

// Anchor is the clock anchor of one source. The zero value is ready to use.
type Anchor struct {
	started  bool
	tps      float64
	base     int64
	lastTick int64
	offset   float64
	// win is a monotonic deque: recv increasing, obs strictly increasing; the front is
	// the minimum of the window.
	win    []anchorObs
	resets int
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
	switch {
	case !a.started:
		a.started, a.tps, a.base = true, tps, 0
		a.restart(recv, tick)
		return
	case tick < a.lastTick:
		// A new game or a reloaded save: start over.
		a.tps, a.base = tps, 0
		a.resets++
		a.restart(recv, tick)
		return
	case tps != a.tps:
		// Re-base at the current tick and continue the clock where it is.
		cont := a.offset + float64(tick-a.base)*1000/a.tps
		a.base, a.tps, a.offset = tick, tps, cont
		a.win = append(a.win[:0], anchorObs{recv, cont})
		a.resets++
	}
	obs := float64(recv) - float64(tick-a.base)*1000/tps
	if math.Abs(obs-a.offset) > AnchorJump {
		a.resets++
		a.restart(recv, tick)
		return
	}
	a.lastTick = tick
	for len(a.win) > 0 && a.win[len(a.win)-1].obs >= obs {
		a.win = a.win[:len(a.win)-1]
	}
	a.win = append(a.win, anchorObs{recv, obs})
	for len(a.win) > 1 && a.win[0].recv < recv-AnchorWindow {
		a.win = a.win[1:]
	}
	if len(a.win) > 64 && cap(a.win) > 4*len(a.win) {
		a.win = append([]anchorObs(nil), a.win...)
	}
	a.offset = a.win[0].obs
}

// restart empties the window and fits the offset to this envelope alone.
func (a *Anchor) restart(recv core.TimeMs, tick int64) {
	obs := float64(recv) - float64(tick-a.base)*1000/a.tps
	a.win = append(a.win[:0], anchorObs{recv, obs})
	a.offset, a.lastTick = obs, tick
}

// T returns the map-clock time of tick. Before the first Observe it returns 0.
func (a *Anchor) T(tick int64) core.TimeMs {
	if !a.started {
		return 0
	}
	return core.TimeMs(math.Round(a.offset + float64(tick-a.base)*1000/a.tps))
}

// Offset returns the current offset in ms (the map-clock time of tick base), for the
// tsdb_clock_anchor_offset_ms metric.
func (a *Anchor) Offset() float64 { return a.offset }

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
