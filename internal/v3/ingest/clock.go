package ingest

import (
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/core"
)

func nowMs() int64 { return time.Now().UnixMilli() }

// DataClock is a map clock that follows the ingested data instead of the wall clock:
// it is the newest envelope recv_ms seen (or the time it was seeded with). A server
// replaying recorded captures (a backlog, a benchmark stream) uses it so the commit loop
// closes windows behind the data rather than behind the wall clock, which would make
// every replayed update late. It is safe for concurrent use.
type DataClock struct{ t atomic.Int64 }

// Observe moves the clock forward to t (never backwards).
func (c *DataClock) Observe(t core.TimeMs) {
	for {
		cur := c.t.Load()
		if int64(t) <= cur || c.t.CompareAndSwap(cur, int64(t)) {
			return
		}
	}
}

// Advance moves the clock forward to t like Observe, but by at most maxJump past its
// current value (when it has one) and never past wall+maxAhead (when maxAhead > 0). It
// reports whether t was cut short. Ingest uses it for envelope recv_ms values, so one
// bad line cannot move the map clock far ahead for good.
func (c *DataClock) Advance(t, wall, maxAhead, maxJump core.TimeMs) (clamped bool) {
	if maxAhead > 0 && t > wall+maxAhead {
		t, clamped = wall+maxAhead, true
	}
	for {
		cur := c.t.Load()
		lim := t
		if cur != 0 && maxJump > 0 && lim > core.TimeMs(cur)+maxJump {
			lim = core.TimeMs(cur) + maxJump
		}
		if int64(lim) <= cur || c.t.CompareAndSwap(cur, int64(lim)) {
			return clamped || lim < t
		}
	}
}

// Now returns the clock.
func (c *DataClock) Now() core.TimeMs { return core.TimeMs(c.t.Load()) }
