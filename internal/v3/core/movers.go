package core

import (
	"context"
	"math"
)

// MoverKind is "t" (train), "v" (vehicle: car, tank, spidertron) or "p" (player).
type MoverKind string

// Mover kinds.
const (
	KindTrain   MoverKind = "t"
	KindVehicle MoverKind = "v"
	KindPlayer  MoverKind = "p"
)

// Valid reports whether k is one of the three kinds.
func (k MoverKind) Valid() bool {
	return k == KindTrain || k == KindVehicle || k == KindPlayer
}

// MoverMeta is the slowly changing description of a journey. A journey whose meta
// changes (a train reverses and its leading carriage becomes a wagon, or gets a
// carriage) gets a new mover table entry with the same ID.
type MoverMeta struct {
	// ID is the journey id. It is stable across train joins and splits and across
	// instance handoffs: "<instance>.<tick>.<train id>" for trains,
	// "<instance>.<tick>.v<unit number>" for vehicles, "p:<player name>" for players.
	ID   string    `json:"id"`
	Kind MoverKind `json:"k"`
	// Force is the mover's force name.
	Force string `json:"f"`
	// Name is the leading carriage's or the vehicle's prototype name, or the player name.
	Name string `json:"name"`
	// Carriages is a train's carriage count (0 for other kinds).
	Carriages int `json:"n,omitempty"`
	// Kinds lists a train's carriages from the leading end: L locomotive, C cargo wagon,
	// F fluid wagon, A artillery wagon; lower case faces backwards. Vehicles: "S"
	// spidertron or "C" car/tank. Empty when unknown.
	Kinds string `json:"o,omitempty"`
	// Color is the leading locomotive's or vehicle's colour as "rrggbb", or "".
	Color string `json:"c,omitempty"`
}

// MoverFlags are per-sample flags (docs/v3/FORMAT.md "MVW3 flags").
type MoverFlags uint8

// Mover sample flags.
const (
	// FlagStop: the mover is at rest at this sample (speed 0).
	FlagStop MoverFlags = 1 << 0
	// FlagGone: the mover left this surface (or this instance) at T. The row carries no
	// position; X, Y and Orientation are 0.
	FlagGone MoverFlags = 1 << 1
	// FlagHandoff: set on a gone row when the same journey has a sample within
	// HandoffWindow after it: an instance crossing, not an end. Viewers keep the path
	// history and bridge the gap.
	FlagHandoff MoverFlags = 1 << 2
	// FlagEndSwitch: the train's leading end flipped at this sample; X, Y are the new
	// leading carriage. Viewers re-anchor instead of sliding the head through the body.
	FlagEndSwitch MoverFlags = 1 << 3
	// FlagNoTick: Tick is unknown (imported data). Velocity must come from T.
	FlagNoTick MoverFlags = 1 << 4
	// FlagNoHeading: Orientation was not captured (imported player and vehicle rows);
	// viewers derive it from the motion. Imported vehicle rows still carry an estimate
	// from the velocity in Orientation, which is wrong for a reversing car or a strafing
	// spidertron.
	FlagNoHeading MoverFlags = 1 << 5
)

// Carriage is one carriage's centre and orientation, part of a train sample.
type Carriage struct {
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Orientation float64 `json:"o"`
}

// MoverSample is one observation of one mover, as producers (ingest, importer) hand it
// to a MoverStore.
type MoverSample struct {
	Surface SurfaceKey
	Meta    MoverMeta
	// T is the map-clock time of the observation.
	T TimeMs
	// Tick is the source's game tick, valid unless Flags has FlagNoTick. Ticks of
	// different sources are unrelated.
	Tick int64
	// X, Y is the position in tiles: a train's leading carriage centre, a vehicle's or
	// player's position.
	X, Y float64
	// Orientation is the Factorio orientation in [0, 1): 0 north, 0.25 east. Trains:
	// from the second carriage towards the leading one.
	Orientation float64
	Flags       MoverFlags
	// Carriages, when present, lists every carriage from the leading end (WP9 sends them
	// on stop, on start, in keyframes and at end switches).
	Carriages []Carriage
}

// Quantization of stored mover positions and orientations (docs/v3/FORMAT.md).
const (
	// PosUnits is the number of position units per tile (1/16 tile).
	PosUnits = 16
	// OrientUnits is the number of orientation units per turn (u16).
	OrientUnits = 65536
)

// QuantizePos rounds a coordinate to the stored grid and returns it in units.
func QuantizePos(v float64) int64 { return int64(math.Round(v * PosUnits)) }

// QuantizeOrientation maps an orientation to [0, 65536) units, wrapping.
func QuantizeOrientation(o float64) uint16 {
	u := int64(math.Round(o * OrientUnits))
	return uint16(((u % OrientUnits) + OrientUnits) % OrientUnits)
}

// Quantized returns s with X, Y, Orientation and carriages rounded exactly as a store
// keeps them. Reading back a stored sample returns Quantized(s).
func (s MoverSample) Quantized() MoverSample {
	q := s
	q.X = float64(QuantizePos(s.X)) / PosUnits
	q.Y = float64(QuantizePos(s.Y)) / PosUnits
	q.Orientation = float64(QuantizeOrientation(s.Orientation)) / OrientUnits
	q.Carriages = nil
	if s.Flags&FlagGone != 0 {
		q.X, q.Y, q.Orientation = 0, 0, 0
	} else if len(s.Carriages) > 0 {
		q.Carriages = make([]Carriage, len(s.Carriages))
		for i, c := range s.Carriages {
			q.Carriages[i] = Carriage{
				X:           float64(QuantizePos(c.X)) / PosUnits,
				Y:           float64(QuantizePos(c.Y)) / PosUnits,
				Orientation: float64(QuantizeOrientation(c.Orientation)) / OrientUnits,
			}
		}
	}
	return q
}

// WindowKind says what a MoverWindow holds.
type WindowKind uint8

// Mover window kinds.
const (
	// WindowClosed is a closed 5-minute window: every row with Start <= T < Start+Len.
	WindowClosed WindowKind = 1
	// WindowKeyframe is the last row of every live mover before Start (Len 0).
	WindowKeyframe WindowKind = 2
	// WindowLive is the live payload at Start ("now"): every row in (Start-Len, Start]
	// plus the last row of every live mover older than that.
	WindowLive WindowKind = 3
)

// MoverRow is one row of a MoverWindow. Mover indexes the window's Movers table.
type MoverRow struct {
	Mover       int
	T           TimeMs
	Tick        int64
	X, Y        float64
	Orientation float64
	Flags       MoverFlags
	Carriages   []Carriage
}

// MoverWindow is the decoded form of an MVW3 body (docs/v3/FORMAT.md): a mover table and
// rows sorted by (Mover, T). Rows with equal Mover and T do not occur (stores keep the
// last arrival).
type MoverWindow struct {
	Surface SurfaceKey
	Kind    WindowKind
	Start   TimeMs
	Len     TimeMs
	Movers  []MoverMeta
	Rows    []MoverRow
}

// WindowRef points at one closed mover window from a day index.
type WindowRef struct {
	// Start is the window start (a multiple of MoverWindowLen); it names the window file.
	Start TimeMs `json:"start"`
	// First and Last are the first and last row times in the window.
	First TimeMs `json:"first"`
	Last  TimeMs `json:"last"`
	// Rows is the number of rows.
	Rows int `json:"rows"`
}

// DayIndex lists one UTC day's closed windows and keyframes for one surface.
type DayIndex struct {
	Date string `json:"date"`
	// Windows are sorted by Start. Only windows with at least one row are listed.
	Windows []WindowRef `json:"windows"`
	// Keyframes are keyframe times (UTC hour boundaries) in the day, sorted.
	Keyframes []TimeMs `json:"keyframes"`
	// Final is true once the surface horizon has passed the end of the day: the index
	// never changes again and may be cached as immutable.
	Final bool `json:"final"`
}

// SurfaceInfo describes one mover surface.
type SurfaceInfo struct {
	Key SurfaceKey `json:"key"`
	ID  string     `json:"id"`
	// Horizon: windows ending at or before it are closed; samples older are late.
	Horizon TimeMs `json:"horizon"`
	FirstT  TimeMs `json:"first"`
	LastT   TimeMs `json:"last"`
	// Days lists the UTC days that have a day index, sorted.
	Days []string `json:"days"`
}

// MoverStore stores mover samples per surface (WP4, internal/v3/movers; reference
// memstore.Movers).
//
// Semantics (normative; memstore.Movers is the executable definition):
//   - Append puts samples into open 5-minute windows (aligned to the epoch). Samples
//     are quantized (MoverSample.Quantized). For equal (journey ID, T) the later one
//     wins. Samples with T < Horizon are Late and dropped; samples with a bad surface
//     key, kind or empty ID are Invalid. FlagHandoff on appended samples is ignored:
//     the store assigns it.
//   - Commit(upTo) closes every window ending at or before upTo and raises the surface
//     Horizon to WindowStart(upTo, MoverWindowLen). When it closes a window it marks each
//     gone row whose journey has a sample in (T, T+HandoffWindow] among all samples
//     known at that moment with FlagHandoff. A window with no rows is not stored.
//   - For every UTC hour boundary h with oldHorizon < h <= newHorizon and FirstT < h it
//     writes a keyframe at h: the last row before h of every journey whose last row
//     before h is not a gone. Keyframe rows are copies of those rows (including their
//     carriages, if any).
//   - Live(now) returns every row with now-LiveTrail < T <= now and, for every live
//     journey whose last row is older, that last row. It reads open and closed data.
//   - Mover tables list one entry per distinct MoverMeta in the window, sorted by
//     (ID, first row T); rows are sorted by (entry, T).
type MoverStore interface {
	Append(ctx context.Context, samples []MoverSample) (ApplyResult, error)
	Commit(ctx context.Context, upTo TimeMs) error
	// Surfaces lists every surface, sorted by ID.
	Surfaces(ctx context.Context) ([]SurfaceInfo, error)
	// Day returns the day index for date ("YYYY-MM-DD"), or ErrNoVersion if the surface
	// has nothing that day.
	Day(ctx context.Context, sfc SurfaceKey, date string) (DayIndex, error)
	// Window returns the closed window starting at start, or ErrNoVersion.
	Window(ctx context.Context, sfc SurfaceKey, start TimeMs) (*MoverWindow, error)
	// Keyframe returns the keyframe at exactly t, or ErrNoVersion.
	Keyframe(ctx context.Context, sfc SurfaceKey, t TimeMs) (*MoverWindow, error)
	// Live returns the live payload at now.
	Live(ctx context.Context, sfc SurfaceKey, now TimeMs) (*MoverWindow, error)
	Close() error
}
