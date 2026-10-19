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

// Now returns the clock.
func (c *DataClock) Now() core.TimeMs { return core.TimeMs(c.t.Load()) }
