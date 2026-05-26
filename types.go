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

	palette    []RGB
	paletteMap map[uint32]uint8
	tiles      [TileCount]*tileState

	totalRows      uint64
	persistedStats *ImportStats
	seqMu          sync.Mutex
	nextSeq        uint64
	skipWAL        bool
	compressor     *compressionPool

	cacheMu       sync.Mutex
	cacheSize     int64
	cacheBytes    int64
	snapshotCache map[snapshotCacheKey]*snapshotCacheEntry
	snapshotOrder []snapshotCacheKey

	closed bool
}

type OpenOptions struct {
	Path      string
	ReadOnly  bool
	CacheSize int64
}

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

func Open(opts OpenOptions) (*DB, error) {
	return openDB(opts)
}

func (db *DB) IngestPlacement(ts time.Time, x, y int, rgb RGB) error {
	return db.ingestPlacement(ts, x, y, rgb)
}

func (db *DB) TileAt(ctx context.Context, opts TileAtOptions) (*TileResult, error) {
	return db.tileAt(ctx, opts)
}

func (db *DB) Close() error {
	return db.close()
}
