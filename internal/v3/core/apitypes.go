package core

// JSON shapes of the v3 HTTP API (docs/v3/API.md) and of the static export, which uses
// the same files. web/src/api/types.ts mirrors these field for field; change both.

// APIVersion is the value of Catalog.Version.
const APIVersion = 1

// Quad is an axis-aligned rectangle or segment [x1, y1, x2, y2] in world tiles.
type Quad [4]float64

// InstanceInfo is one Clusterio instance on a map (gridworld tile bounds).
type InstanceInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Bounds *Quad  `json:"bounds,omitempty"`
}

// MapInfo is per-map metadata the forwarder sends in "map" messages
// (docs/v3/PROTOCOL.md) and the catalog repeats.
type MapInfo struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// TileSize is the gridworld tile size in tiles (512), 0 when not a gridworld map.
	TileSize  int            `json:"tile_size,omitempty"`
	Instances []InstanceInfo `json:"instances"`
	// Edges maps a surface name to its Universal Edges border segments.
	Edges map[string][]Quad `json:"edges,omitempty"`
}

// CatalogDatastore is one terrain datastore in the catalog.
type CatalogDatastore struct {
	Force          string   `json:"force"`
	ID             string   `json:"id"`
	WindowMs       TimeMs   `json:"window_ms"`
	Horizon        TimeMs   `json:"horizon"`
	First          TimeMs   `json:"first"`
	Last           TimeMs   `json:"last"`
	PaletteVersion uint32   `json:"palette_version"`
	Bounds         [4]int32 `json:"bounds"`
	// Levels lists the levels that have tiles, from 0 down (0, -1, ...).
	Levels []int8 `json:"levels"`
}

// CatalogMovers describes a surface's mover data in the catalog.
type CatalogMovers struct {
	Horizon TimeMs   `json:"horizon"`
	First   TimeMs   `json:"first"`
	Last    TimeMs   `json:"last"`
	Days    []string `json:"days"`
}

// CatalogSurface is one surface of a map in the catalog.
type CatalogSurface struct {
	Name string `json:"name"`
	// ID is the surface id (SurfaceKey.ID), used by mover routes.
	ID         string             `json:"id"`
	Edges      []Quad             `json:"edges"`
	Datastores []CatalogDatastore `json:"datastores"`
	Movers     *CatalogMovers     `json:"movers"`
}

// CatalogMap is one map in the catalog.
type CatalogMap struct {
	MapInfo
	Surfaces []CatalogSurface `json:"surfaces"`
}

// CatalogIcons points at the icon spritesheet and its metadata (paths relative to the
// API base), or is absent when no icons were published.
type CatalogIcons struct {
	PNG  string `json:"png"`
	JSON string `json:"json"`
}

// Catalog is GET catalog.
type Catalog struct {
	Version int `json:"version"`
	// Now is the server's map clock when the catalog was made (static export: export
	// time). Viewers use it, with X-TSDB-Now headers, as their data clock.
	Now TimeMs `json:"now"`
	// Static is true for a static export (no view, live.bin tiles or SSE).
	Static bool `json:"static"`
	// LiveMs is the recommended poll interval for mutable files (static export).
	LiveMs int           `json:"live_ms"`
	Maps   []CatalogMap  `json:"maps"`
	Icons  *CatalogIcons `json:"icons"`
}

// ViewTile is one tile in a view response.
type ViewTile struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	TileState
}

// ViewResponse is GET view/{ds}/{z}.
type ViewResponse struct {
	DS      string     `json:"ds"`
	Z       int8       `json:"z"`
	T       TimeMs     `json:"t"`
	Horizon TimeMs     `json:"horizon"`
	Tiles   []ViewTile `json:"tiles"`
}

// TimelineTile is one tile's version history for one UTC day.
type TimelineTile struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	// V0 is the version effective at the start of the day.
	V0 Version `json:"v0"`
	// Points are [t, version] pairs for versions that became effective during the day.
	Points [][2]int64 `json:"points"`
}

// TimelineResponse is GET timeline/{ds}/{z}/{date}.json.
type TimelineResponse struct {
	DS    string         `json:"ds"`
	Z     int8           `json:"z"`
	Date  string         `json:"date"`
	Start TimeMs         `json:"start"`
	Final bool           `json:"final"`
	Tiles []TimelineTile `json:"tiles"`
}

// OverlayResponse is GET overlays/{ds}/{x}/{y}.json.
type OverlayResponse struct {
	DS        string     `json:"ds"`
	X         int32      `json:"x"`
	Y         int32      `json:"y"`
	Version   uint64     `json:"version"`
	Intervals []Interval `json:"intervals"`
}

// ApplyCounts is ApplyResult under its API name.
type ApplyCounts = ApplyResult

// IngestResponse is the body of a successful POST ingest.
type IngestResponse struct {
	Envelopes int         `json:"envelopes"`
	BadLines  int         `json:"bad_lines"`
	Chunks    ApplyCounts `json:"chunks"`
	Samples   ApplyCounts `json:"samples"`
	Overlays  ApplyCounts `json:"overlays"`
	// Errors holds up to 10 messages about bad lines or messages.
	Errors []string `json:"errors"`
	// Seq echoes the highest envelope seq seen in the body, for tailer acknowledgements.
	Seq *int64 `json:"seq,omitempty"`
	// Duplicates counts envelopes skipped because their source had them applied already
	// (a retried body); OneRecv envelopes stamped with their source's previous receive
	// time although their tick was more than two seconds later (a forwarder that lost the
	// time, API.md 5).
	Duplicates int `json:"duplicates,omitempty"`
	OneRecv    int `json:"one_recv,omitempty"`
}

// ErrorResponse is the body of every 4xx/5xx JSON error.
type ErrorResponse struct {
	Error string `json:"error"`
	// Code is a stable machine-readable code: bad_request, bad_tile, not_found,
	// unauthorized, too_large, overloaded, internal.
	Code string `json:"code"`
}
