package core

import "context"

// IntervalKind is the kind of an overlay interval.
type IntervalKind string

// Overlay interval kinds.
const (
	// OverlayTag is a chart tag (icon and text).
	OverlayTag IntervalKind = "tag"
	// OverlayRecipe is an assembling machine's or furnace's recipe and quality.
	OverlayRecipe IntervalKind = "recipe"
	// OverlayRail is a rail entity (optional, from WP9 capture), used for map-matching.
	OverlayRail IntervalKind = "rail"
)

// Valid reports whether k is a known kind.
func (k IntervalKind) Valid() bool {
	return k == OverlayTag || k == OverlayRecipe || k == OverlayRail
}

// Interval is one version of one overlay object, valid in [Start, End). End == 0 means
// still open.
type Interval struct {
	Kind IntervalKind `json:"kind"`
	// Key identifies the object within (datastore, kind): "<instance>/<tag number>" for
	// tags, "<instance>/<unit number>" for recipes and rails. Standalone sources use
	// instance 0.
	Key string `json:"key"`
	// Instance is the Clusterio instance id that reported the object (0 standalone).
	Instance int64  `json:"instance"`
	Start    TimeMs `json:"start"`
	End      TimeMs `json:"end"`
	// X, Y is the object position in tiles (machine or rail centre, tag position).
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Tag fields: Icon is "<signal type>/<name>" or "", Text the tag text.
	Icon string `json:"icon,omitempty"`
	Text string `json:"text,omitempty"`
	// Recipe fields.
	Recipe  string `json:"recipe,omitempty"`
	Quality string `json:"quality,omitempty"`
	// Rail fields: Name is the rail prototype, Direction the entity direction (0..15).
	Name      string `json:"name,omitempty"`
	Direction uint8  `json:"dir,omitempty"`
}

// Open reports whether the interval has no end yet.
func (iv Interval) Open() bool { return iv.End == 0 }

// Overlaps reports whether the interval intersects [t0, t1).
func (iv Interval) Overlaps(t0, t1 TimeMs) bool {
	return iv.Start < t1 && (iv.End == 0 || iv.End > t0)
}

// SameContent reports whether two intervals describe the same object state (all fields
// except Start and End).
func (iv Interval) SameContent(o Interval) bool {
	a, b := iv, o
	a.Start, a.End, b.Start, b.End = 0, 0, 0, 0
	return a == b
}

// OverlayEvent opens or closes an overlay object at time T.
//
// An open event (Close false) for Key: if an interval for Key is open with the same
// content, nothing happens; otherwise the open interval (if any) ends at T and a new
// one starts at T. A close event ends the open interval for Key at T; without one it
// is ignored. Interval.Start and End in the event are ignored.
type OverlayEvent struct {
	DS       DatastoreKey
	T        TimeMs
	Close    bool
	Interval Interval
}

// OverlayQuery selects intervals.
type OverlayQuery struct {
	// Kinds restricts the kinds; empty means all.
	Kinds []IntervalKind
	// Tile is the z0 tile the object positions must fall in.
	Tile TileCoord
	// T0, T1: intervals overlapping [T0, T1). T1 <= T0 means T1 = T0 + 1 (a point query).
	T0, T1 TimeMs
}

// OverlayStore stores overlay intervals (WP4, internal/v3/overlays; reference
// memstore.Overlays).
//
// Events for one key must arrive in T order; an event older than the start of the
// key's current interval is Late and dropped. Events with an unknown kind, empty key
// or bad datastore key are Invalid.
type OverlayStore interface {
	Apply(ctx context.Context, events []OverlayEvent) (ApplyResult, error)
	// Query returns the matching intervals sorted by (Kind, Key, Start). ErrBadTile for a
	// tile that is not a valid z0 tile; an unknown datastore returns no intervals.
	Query(ctx context.Context, ds DatastoreKey, q OverlayQuery) ([]Interval, error)
	// Version returns a counter that changes whenever an interval in the z0 tile is
	// added or ended. 0 when the tile has none. It keys caches and ETags.
	Version(ctx context.Context, ds DatastoreKey, tile TileCoord) (uint64, error)
	Close() error
}
