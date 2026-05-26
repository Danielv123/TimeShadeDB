package timeshadedb

import (
	"context"
	"sync"
	"time"
)

const (
	CanvasWidth  = 2000
	CanvasHeight = 2000
	TileSize     = 512
	TileCols     = 4
	TileRows     = 4
	TileCount    = TileCols * TileRows
)

type RGB struct {
	R uint8
	G uint8
	B uint8
}

type DB struct {
	path     string
	readOnly bool
	format   string

	palette    []RGB
	paletteMap map[uint32]uint8
	tiles      [TileCount]*tileState

	totalRows      uint64
	persistedStats *ImportStats
	seqMu          sync.Mutex
	nextSeq        uint64
	skipWAL        bool
	batchMode      bool
	compressor     *compressionPool

	cacheMu       sync.Mutex
	cacheSize     int64
	cacheBytes    int64
	snapshotCache map[snapshotCacheKey]*snapshotCacheEntry
	snapshotOrder []snapshotCacheKey

	chunkMu         sync.RWMutex
	chunkLoaded     bool
	chunkDatastores map[DatastoreKey]*chunkDatastore
	chunkTileLocks  sync.Map
	nextChunkSeq    uint64

	closed bool
}

type OpenOptions struct {
	Path      string
	ReadOnly  bool
	CacheSize int64
	Format    string
}

const (
	FormatLegacyTiles = "legacy-tiles"
	FormatChunks      = "chunks"
)

type TileCoord struct {
	X int
	Y int
}

type TileAtOptions struct {
	Timestamp time.Time
	Tile      TileCoord
}

type TileResult struct {
	Tile        TileCoord
	Width       int
	Height      int
	Palette     []RGB
	Pixels      []uint8
	SnapshotSec uint32
	Replayed    int
}

type DatastoreKey struct {
	SavefileUUID string `json:"savefile_uuid"`
	Surface      string `json:"surface"`
	Force        string `json:"force"`
}

type ChunkCoord struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

type ChunkIngest struct {
	Key    DatastoreKey
	Tick   uint64
	Chunk  ChunkCoord
	Pixels []uint16
}

type IngestChunkResult struct {
	AcceptedRows      uint64 `json:"accepted_rows"`
	ChangedPixels     uint64 `json:"changed_pixels"`
	UnchangedPixels   uint64 `json:"unchanged_pixels"`
	DatastoresTouched int    `json:"datastores_touched"`
	LatestRowSeq      uint64 `json:"latest_row_seq,omitempty"`
}

type ChunkAtOptions struct {
	Key   DatastoreKey
	Tick  uint64
	Chunk ChunkCoord
}

type ChunkResult struct {
	Key          DatastoreKey
	Chunk        ChunkCoord
	Tick         uint64
	Width        int
	Height       int
	Pixels       []uint16
	SnapshotTick uint64
	Replayed     int
}

type IngestMetadata struct {
	SavefileUUID string                    `json:"savefile_uuid"`
	Datastores   []IngestDatastoreMetadata `json:"datastores"`
}

type IngestDatastoreMetadata struct {
	Surface      string `json:"surface"`
	Force        string `json:"force"`
	LatestTick   uint64 `json:"latest_tick"`
	LatestChunkX int32  `json:"latest_chunk_x"`
	LatestChunkY int32  `json:"latest_chunk_y"`
	LatestRowSeq uint64 `json:"latest_row_seq"`
}

func Open(opts OpenOptions) (*DB, error) {
	return openDB(opts)
}

func (db *DB) IngestPlacement(ts time.Time, x, y int, rgb RGB) error {
	return db.ingestPlacement(ts, x, y, rgb)
}

func (db *DB) IngestChunk(ctx context.Context, in ChunkIngest) (*IngestChunkResult, error) {
	return db.ingestChunk(ctx, in)
}

func (db *DB) IngestChunkRows(ctx context.Context, rows []ParsedChunkRow) (*IngestChunkResult, error) {
	return db.ingestChunkRows(ctx, rows)
}

func (db *DB) TileAt(ctx context.Context, opts TileAtOptions) (*TileResult, error) {
	return db.tileAt(ctx, opts)
}

func (db *DB) ChunkAt(ctx context.Context, opts ChunkAtOptions) (*ChunkResult, error) {
	return db.chunkAt(ctx, opts)
}

func (db *DB) IngestMetadata(savefileUUID string) (*IngestMetadata, error) {
	return db.ingestMetadata(savefileUUID)
}

func (db *DB) TimeRange() (uint32, uint32) {
	return db.timeRange()
}

func (db *DB) Close() error {
	return db.close()
}
