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
//
// It keeps two values. Seen is the newest recv_ms read from a body; ingest times
// messages against it. Now, the value the commit loop closes windows behind, only moves
// to a seen value once the rows read before it were applied to the stores (Publish).
// Otherwise a body spanning more data time than the commit grace could let the commit
// loop close a window between reading an envelope and applying its rows, and the store
// would count those rows as late.
type DataClock struct{ t, seen atomic.Int64 }

func maxStore(v *atomic.Int64, t int64) {
	for {
		cur := v.Load()
		if t <= cur || v.CompareAndSwap(cur, t) {
			return
		}
	}
}

// Observe moves the clock forward to t (never backwards), both seen and published.
func (c *DataClock) Observe(t core.TimeMs) {
	maxStore(&c.seen, int64(t))
	maxStore(&c.t, int64(t))
}

// Advance moves the seen clock forward to t, but by at most maxJump past its current
// value (when it has one) and never past wall+maxAhead (when maxAhead > 0). It reports
// whether t was cut short. Ingest uses it for envelope recv_ms values, so one bad line
// cannot move the map clock far ahead for good. Now follows at the next Publish.
func (c *DataClock) Advance(t, wall, maxAhead, maxJump core.TimeMs) (clamped bool) {
	if maxAhead > 0 && t > wall+maxAhead {
		t, clamped = wall+maxAhead, true
	}
	cur := max(c.seen.Load(), c.t.Load())
	lim := t
	if cur != 0 && maxJump > 0 && lim > core.TimeMs(cur)+maxJump {
		lim = core.TimeMs(cur) + maxJump
	}
	maxStore(&c.seen, int64(lim))
	return clamped || lim < t
}

// Seen returns the newest recv_ms read so far (at least Now).
func (c *DataClock) Seen() core.TimeMs { return core.TimeMs(max(c.seen.Load(), c.t.Load())) }

// Publish moves Now forward to t (a value Seen returned once the rows read before it
// were applied).
func (c *DataClock) Publish(t core.TimeMs) { maxStore(&c.t, int64(t)) }

// Now returns the clock the commit loop uses: the newest published value.
func (c *DataClock) Now() core.TimeMs { return core.TimeMs(c.t.Load()) }
