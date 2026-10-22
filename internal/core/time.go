package core

import (
	"fmt"
	"time"
)

// TimeMs is the v3 primary time key: Unix milliseconds on the map clock (PLAN.md §4.1, D2).
//
// A gridworld map spans many instances with separate game ticks, so wall-clock time is
// the only clock they share. Live capture derives TimeMs from the game tick through a
// per-source anchor (docs/PROTOCOL.md "Clock anchors"); imported data uses the
// milliseconds it was stored with.
type TimeMs int64

// Common durations in milliseconds.
const (
	Second TimeMs = 1000
	Minute TimeMs = 60 * Second
	Hour   TimeMs = 60 * Minute
	Day    TimeMs = 24 * Hour
)

// Time window defaults (PLAN.md D3, §4.4).
const (
	// DefaultTerrainWindow is the terrain net-change window length.
	DefaultTerrainWindow TimeMs = 60 * Second
	// MoverWindowLen is the length of a closed mover window file.
	MoverWindowLen TimeMs = 5 * Minute
	// MoverKeyframeEvery is the spacing of mover keyframes (aligned to UTC hours).
	MoverKeyframeEvery TimeMs = Hour
	// LiveTrail is how far back the live mover payload keeps every sample.
	LiveTrail TimeMs = 60 * Second
	// HandoffWindow: a gone followed by a sample of the same journey within this time is
	// a handoff (instance crossing), not an end.
	HandoffWindow TimeMs = 10 * Second
)

// FloorDiv returns floor(a / b) for b > 0.
func FloorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// WindowStart returns the start of the window of length w that contains t.
// Windows are aligned to the Unix epoch: [k*w, (k+1)*w).
func WindowStart(t, w TimeMs) TimeMs {
	return TimeMs(FloorDiv(int64(t), int64(w)) * int64(w))
}

// WindowEnd returns the exclusive end of the window of length w that contains t.
func WindowEnd(t, w TimeMs) TimeMs {
	return WindowStart(t, w) + w
}

// Time converts to a time.Time in UTC.
func (t TimeMs) Time() time.Time {
	return time.UnixMilli(int64(t)).UTC()
}

// FromTime converts a time.Time to TimeMs.
func FromTime(t time.Time) TimeMs {
	return TimeMs(t.UnixMilli())
}

// UTCDay returns the UTC day containing t as "YYYY-MM-DD", the name of mover day indexes.
func UTCDay(t TimeMs) string {
	return t.Time().Format("2006-01-02")
}

// ParseUTCDay parses "YYYY-MM-DD" and returns the start of that UTC day.
func ParseUTCDay(s string) (TimeMs, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("bad UTC day %q: %w", s, err)
	}
	return FromTime(d), nil
}

// String formats t as RFC 3339 with milliseconds, for logs and errors.
func (t TimeMs) String() string {
	return t.Time().Format("2006-01-02T15:04:05.000Z")
}
