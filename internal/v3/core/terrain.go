package core

import (
	"context"
	"errors"
)

// Errors shared by the stores. Wrap them with fmt.Errorf("...: %w", Err...) and test
// with errors.Is. docs/v3/API.md maps them to HTTP status codes.
var (
	// ErrUnknownDatastore: no datastore or surface with that key exists. HTTP 404.
	ErrUnknownDatastore = errors.New("unknown datastore")
	// ErrNoTile: the tile is valid but holds no data. HTTP 404.
	ErrNoTile = errors.New("no such tile")
	// ErrBadTile: coordinates outside the grid (TileCoord.Valid is false) or the wrong
	// level for the call. HTTP 400. Stores must check this before allocating anything.
	ErrBadTile = errors.New("bad tile coordinates")
	// ErrNoVersion: the tile exists but not at that version (or window, keyframe). HTTP 404.
	ErrNoVersion = errors.New("no such version")
	// ErrClosed: the store was closed.
	ErrClosed = errors.New("store closed")
)

// Version identifies one committed state of one tile. At z0 it counts the tile's
// committed blocks that changed content: version 0 is "nothing committed" (all
// uncharted), and each later version is the state after one more block. Versions only
// grow, and the content of (datastore, tile, version) never changes, so it can be
// served from an immutable URL. Coarse levels (z < 0) number their snapshots the same
// way. A version is only meaningful together with its tile.
type Version uint32

// ChunkUpdate is one chart observation of one chunk (PLAN.md §4.2 write path).
type ChunkUpdate struct {
	DS    DatastoreKey
	Chunk ChunkCoord
	T     TimeMs
	// Deleted marks the chunk as removed (Lua sent no chart). It sets all pixels to 0.
	Deleted bool
	// Pixels is the full chart of the chunk. It must be non-nil unless Deleted. Stores
	// copy it; callers may reuse the array after Apply returns.
	Pixels *ChunkPixels
}

// ApplyResult counts what Apply did with a batch.
type ApplyResult struct {
	// Accepted updates went into an open window.
	Accepted int `json:"accepted"`
	// Late updates were older than the datastore's committed horizon and were dropped.
	// Terrain is absolute, so the next update of that chunk corrects the state.
	Late int `json:"late"`
	// Invalid updates had a bad key, out-of-grid chunk or nil pixels, and were dropped.
	Invalid int `json:"invalid"`
}

// Add accumulates another result.
func (r *ApplyResult) Add(o ApplyResult) {
	r.Accepted += o.Accepted
	r.Late += o.Late
	r.Invalid += o.Invalid
}

// TileState is a tile's state as seen at one time t.
type TileState struct {
	// Version is the committed version effective at t.
	Version Version `json:"v"`
	// Live is true when the tile has open-window updates and t is at or after both the
	// datastore Horizon and its LastT (the newest accepted update time). The live content
	// (TerrainStore.ReadTile with live=true) may then differ from Version's content.
	// Live content is not offered for earlier instants, because open windows keep only
	// the newest update per chunk.
	Live bool `json:"live,omitempty"`
	// LiveSeq changes whenever the open-window content of the tile changes; clients use
	// it to skip refetching unchanged live content. 0 when not Live.
	LiveSeq uint64 `json:"ls,omitempty"`
}

// VersionPoint says that Version became effective at time T.
type VersionPoint struct {
	T       TimeMs  `json:"t"`
	Version Version `json:"v"`
}

// TileInfo describes one tile that holds data.
type TileInfo struct {
	Tile TileCoord `json:"tile"`
	// Version is the latest committed version.
	Version Version `json:"v"`
	// FirstT is when version 1 became effective, LastT when Version did. Both are 0 for a
	// tile that has only open-window data.
	FirstT TimeMs `json:"first"`
	LastT  TimeMs `json:"last"`
	// Live is true when the open window holds updates for the tile.
	Live bool `json:"live,omitempty"`
}

// ChunkDelta is one chunk of a tile delta: block b's full content at the newer version.
type ChunkDelta struct {
	Block  uint8
	Pixels ChunkPixels
}

// Palette is a datastore's append-only colour table (docs/v3/FORMAT.md "Palette").
// Colors[0] is always 0 (uncharted). Entries are never changed or removed, so a
// palette prefix of length n is valid for every tile encoded with palette version n.
// The order of colours is store-defined: different stores fed the same data may number
// colours differently. Compare tiles as RGB565, never as palette indices.
type Palette struct {
	Colors []uint16
}

// Version is the palette version: its number of entries.
func (p Palette) Version() uint32 { return uint32(len(p.Colors)) }

// DatastoreInfo describes one terrain datastore.
type DatastoreInfo struct {
	Key DatastoreKey `json:"key"`
	ID  string       `json:"id"`
	// Window is the net-change window length, fixed when the datastore is created.
	Window TimeMs `json:"window_ms"`
	// Horizon is the committed horizon: every window ending at or before it is closed.
	// Updates with T < Horizon are late and dropped. Tile versions at times before
	// Horizon never change.
	Horizon TimeMs `json:"horizon"`
	// FirstT and LastT are the oldest and newest accepted update times (0 if none).
	FirstT TimeMs `json:"first"`
	LastT  TimeMs `json:"last"`
	// PaletteVersion is the current palette length.
	PaletteVersion uint32 `json:"palette_version"`
	// Tiles is the number of z0 tiles holding data.
	Tiles int `json:"tiles"`
	// Bounds is the z0 tile bounding box [minX, minY, maxX, maxY], inclusive. Zero when
	// Tiles is 0.
	Bounds [4]int32 `json:"bounds"`
}

// TileSource reads versioned tiles at one or more levels. TerrainStore implements it
// for z0 (other levels return ErrBadTile); the pyramid (WP3, internal/v3/pyramid)
// implements it for every level z <= 0, delegating z0 to a TerrainStore. The HTTP API
// (WP6) and the static exporter (WP12) only use this interface for reads.
//
// Versions effective before the datastore Horizon never change once served: new versions
// become effective at or after the Horizon, except in offline builds (import, pyramid
// rebuild) that run before the database is served. Coarse levels are built after the
// commit that moves the Horizon, so for z < 0 the bound is min(Horizon, the pyramid's
// BuiltUpTo): VersionAt(t) for t in [BuiltUpTo, Horizon) can still change (API.md "view").
//
// Every method returns ErrBadTile for tiles outside the grid or level, ErrUnknownDatastore
// for unknown datastores and ErrNoTile for valid tiles that hold no data, and must not
// allocate per-tile state for any of these.
type TileSource interface {
	// VersionAt returns the tile's state at time t: the latest version whose effective
	// time is <= t (version 0 if none), and the live flags.
	VersionAt(ctx context.Context, ds DatastoreKey, tile TileCoord, t TimeMs) (TileState, error)
	// ReadVersion returns the content of exactly that version. Version 0 is all
	// uncharted. Versions above the latest return ErrNoVersion.
	ReadVersion(ctx context.Context, ds DatastoreKey, tile TileCoord, v Version) (*TileImage, error)
	// History returns the versions that became effective in [t0, t1), oldest first.
	History(ctx context.Context, ds DatastoreKey, tile TileCoord, t0, t1 TimeMs) ([]VersionPoint, error)
	// ListTiles returns every tile at level z that holds data, sorted by (y, x).
	ListTiles(ctx context.Context, ds DatastoreKey, z int8) ([]TileInfo, error)
}

// TerrainStore is the z0 terrain store (WP2, internal/v3/terrain; reference
// memstore.Terrain).
//
// Semantics (normative; memstore.Terrain is the executable definition):
//   - Each datastore has a window length W. An update at time T belongs to window
//     [WindowStart(T, W), WindowEnd(T, W)).
//   - Apply puts updates into open windows. Per chunk and window the update with the
//     greatest T wins; on equal T the later Apply call (or later element) wins.
//     Updates with T < Horizon are counted as Late and dropped.
//   - Commit(upTo) closes, in time order, every open window that ends at or before upTo
//     and raises every datastore's Horizon to WindowStart(upTo, W) (Horizon never
//     moves backwards). For each tile and closed window, the chunks whose final content
//     differs from the committed content form one block; a non-empty block creates the
//     next Version, effective at the window end. Updates that net out (A->B->A) create
//     nothing.
//   - A store may flush a window early under memory pressure. It may then create more
//     than one version for the same window, all effective at the window end. Readers
//     see the same pixels at every t; only the version numbers differ.
//   - Without live, reads see committed content only: the state at time t is the
//     content of VersionAt(t).Version. With live, and when TileState.Live holds at t
//     (t >= Horizon and t >= LastT), reads also apply the newest open-window update of
//     each chunk. Clients in live mode ask with t = "latest" (the API resolves it to
//     max(now, LastT)).
type TerrainStore interface {
	TileSource

	// Apply ingests chunk updates. It returns an error only for failures of the store
	// itself (I/O); bad and late updates are counted in the result.
	Apply(ctx context.Context, updates []ChunkUpdate) (ApplyResult, error)
	// Commit closes every open window that ends at or before upTo, in every datastore.
	// The server calls it periodically with the map clock minus a grace period.
	Commit(ctx context.Context, upTo TimeMs) error
	// EnsureDatastore creates the datastore with window length w if it does not exist
	// (w <= 0 means DefaultTerrainWindow). Apply creates unknown datastores with the
	// default window, so calling this is only needed for a non-default window.
	EnsureDatastore(ctx context.Context, ds DatastoreKey, w TimeMs) error
	// Datastores lists every datastore, sorted by ID.
	Datastores(ctx context.Context) ([]DatastoreInfo, error)
	// Palette returns a copy of the datastore's current palette.
	Palette(ctx context.Context, ds DatastoreKey) (Palette, error)
	// ReadTile returns the z0 tile's content at time t (see the live rule above) and the
	// state it was read at. With live false the returned state has Live false and
	// LiveSeq 0, whatever VersionAt would say.
	ReadTile(ctx context.Context, ds DatastoreKey, tile TileCoord, t TimeMs, live bool) (*TileImage, TileState, error)
	// ChangedChunks returns the blocks whose content differs between versions from and
	// to (from < to), with their content at version to, sorted by block. It backs the
	// TSD3 tile delta.
	ChangedChunks(ctx context.Context, ds DatastoreKey, tile TileCoord, from, to Version) ([]ChunkDelta, error)
	// Close flushes nothing that is not already durable by the store's rules and
	// releases resources. Open windows are kept in the WAL (WP2) or dropped (memstore).
	Close() error
}
