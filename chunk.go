package timeshadedb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	ChunkSize           = 32
	ChunkPixelCount     = ChunkSize * ChunkSize
	chunkDatastoresDir  = "datastores"
	chunkPayloadHex     = ChunkPixelCount * 2 * 2
	chunkPayloadBytes   = ChunkPixelCount * 2
	chunkFrameHeaderLen = uint16(37)
	chunkCopyBatchRows  = 4096

	chunkSnapshotMaxDeltaEvents = 1024
	chunkSnapshotMaxDeltaFrames = 256
)

type chunkDatastore struct {
	key             DatastoreKey
	chunks          map[ChunkCoord]*factorioChunkState
	events          map[ChunkCoord][]chunkLogEntry
	latestTick      uint64
	latestChunk     ChunkCoord
	latestRowSeq    uint64
	dirsReady       bool
	metadataWritten bool
}

type chunkDatastoreProgress struct {
	LatestTick   uint64 `json:"latest_tick"`
	LatestChunkX int32  `json:"latest_chunk_x"`
	LatestChunkY int32  `json:"latest_chunk_y"`
	LatestRowSeq uint64 `json:"latest_row_seq"`
}

type factorioChunkState struct {
	pixels                  [ChunkPixelCount]uint16
	seen                    bool
	latestTick              uint64
	latestRowSeq            uint64
	index                   chunkIndex
	tileBacked              bool
	lastSnapshotBytes       uint32
	deltaFramesSinceSnap    uint32
	deltaEventsSinceSnap    uint32
	deltaBytesSinceSnapshot uint64
}

type chunkIndex struct {
	snapshots []chunkSnapshotRecord
	deltas    []chunkDeltaRecord
}

type chunkSnapshotRecord struct {
	Tick               uint64
	FrameOffset        uint64
	FrameHeaderLen     uint16
	CompressedLen      uint32
	RawLen             uint32
	FirstDeltaFrameIdx uint32
	EventSeq           uint64
	Checksum           uint32
}

type chunkDeltaRecord struct {
	MinTick        uint64
	MaxTick        uint64
	FrameOffset    uint64
	FrameHeaderLen uint16
	CompressedLen  uint32
	RawLen         uint32
	EventCount     uint32
	FirstEventSeq  uint64
	LastEventSeq   uint64
	Checksum       uint32
}

type chunkFrameInfo struct {
	kind          uint8
	tick          uint64
	maxTick       uint64
	eventCount    uint32
	rawLen        uint32
	compressedLen uint32
	checksum      uint32
	payload       []byte
}

type chunkTileCoord struct {
	X int32
	Y int32
}

type chunkWrite struct {
	ds             *chunkDatastore
	coord          ChunkCoord
	chunk          *factorioChunkState
	entry          chunkLogEntry
	snapshot       bool
	snapshotPixels [ChunkPixelCount]uint16
}

type chunkPixelChange struct {
	Pos   uint16 `json:"pos"`
	Color uint16 `json:"color"`
}

type chunkLogEntry struct {
	Seq     uint64             `json:"seq"`
	Tick    uint64             `json:"tick"`
	Key     DatastoreKey       `json:"key"`
	Chunk   ChunkCoord         `json:"chunk"`
	Changes []chunkPixelChange `json:"changes"`
}

type ParsedChunkRow struct {
	Key        DatastoreKey
	Tick       uint64
	Chunk      ChunkCoord
	Pixels     [ChunkPixelCount]uint16
	PayloadHex string
	HasPayload bool
}

type chunkIngestWork struct {
	row   ParsedChunkRow
	seq   uint64
	ds    *chunkDatastore
	chunk *factorioChunkState
}

type chunkTileBatchKey struct {
	key   DatastoreKey
	tileX int32
	tileY int32
}

type chunkTileBatchResult struct {
	result   IngestChunkResult
	touched  map[DatastoreKey]struct{}
	progress map[DatastoreKey]chunkDatastoreProgress
	events   map[DatastoreKey]map[ChunkCoord][]chunkLogEntry
	err      error
}

type chunkCopyDatastore struct {
	key          DatastoreKey
	latestTick   uint64
	latestChunk  ChunkCoord
	latestRowSeq uint64
	events       map[ChunkCoord][]chunkLogEntry
}

type chunkReadPlan struct {
	coord ChunkCoord
	path  string
	tile  chunkTileCoord
	idx   chunkIndex
}

type chunkFrameRead struct {
	coord  ChunkCoord
	path   string
	tile   chunkTileCoord
	offset uint64
	kind   uint8
}

type chunkReplayState struct {
	pixels       [ChunkPixelCount]uint16
	seen         bool
	snapshotTick uint64
	replayed     int
}

func createChunkDB(path string, cacheSize int64) (*DB, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	db := &DB{
		path:            path,
		format:          FormatChunks,
		compressor:      newCompressionPool(),
		cacheSize:       cacheSize,
		chunkDatastores: map[DatastoreKey]*chunkDatastore{},
		nextChunkSeq:    1,
		chunkLoaded:     true,
	}
	if err := writeChunkManifest(path); err != nil {
		_ = db.close()
		return nil, err
	}
	return db, nil
}

func loadChunkDB(opts OpenOptions) (*DB, error) {
	db := &DB{
		path:            opts.Path,
		readOnly:        opts.ReadOnly,
		format:          FormatChunks,
		cacheSize:       opts.CacheSize,
		chunkDatastores: map[DatastoreKey]*chunkDatastore{},
		nextChunkSeq:    1,
	}
	if !opts.ReadOnly {
		db.compressor = newCompressionPool()
	}
	if err := db.loadChunksLocked(); err != nil {
		_ = db.close()
		return nil, err
	}
	return db, nil
}

func writeChunkManifest(path string) error {
	m := baseDataManifest(2)
	m.ChunkSize = ChunkSize
	m.PixelFormat = "rgb565"
	m.TimestampUnit = "factorio_tick"
	return writeDataManifest(path, m)
}

func (db *DB) ingestChunk(ctx context.Context, in ChunkIngest) (*IngestChunkResult, error) {
	if len(in.Pixels) != ChunkPixelCount {
		return nil, fmt.Errorf("timeshadedb: chunk has %d pixels, expected %d", len(in.Pixels), ChunkPixelCount)
	}
	var pixels [ChunkPixelCount]uint16
	copy(pixels[:], in.Pixels)
	return db.ingestChunkRows(ctx, []ParsedChunkRow{{
		Key:    in.Key,
		Tick:   in.Tick,
		Chunk:  in.Chunk,
		Pixels: pixels,
	}})
}

func (db *DB) ingestChunkRows(ctx context.Context, rows []ParsedChunkRow) (*IngestChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &IngestChunkResult{}, nil
	}
	if db.closed {
		return nil, errors.New("timeshadedb: database is closed")
	}
	if db.readOnly {
		return nil, errors.New("timeshadedb: database opened read-only")
	}
	if db.format == FormatLegacyTiles {
		return nil, errors.New("timeshadedb: chunk ingest requires a chunk database")
	}
	for _, row := range rows {
		if err := validateDatastoreKey(row.Key); err != nil {
			return nil, err
		}
	}

	db.chunkMu.Lock()
	if err := db.loadChunksLocked(); err != nil {
		db.chunkMu.Unlock()
		return nil, err
	}
	db.chunkMu.Unlock()

	touched := map[DatastoreKey]struct{}{}
	touchedDatastores := map[DatastoreKey]*chunkDatastore{}
	rowGroups := map[chunkTileBatchKey][]ParsedChunkRow{}
	groupOrder := make([]chunkTileBatchKey, 0)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := chunkTileKey(row.Key, row.Chunk)
		if _, ok := rowGroups[key]; !ok {
			groupOrder = append(groupOrder, key)
		}
		rowGroups[key] = append(rowGroups[key], row)
	}
	releaseTileLocks := db.acquireChunkTileLocks(groupOrder)
	defer releaseTileLocks()

	groups := map[chunkTileBatchKey][]chunkIngestWork{}
	db.chunkMu.Lock()
	for _, key := range groupOrder {
		for _, row := range rowGroups[key] {
			ds := db.chunkDatastoreLocked(row.Key)
			chunk := ds.chunkLocked(row.Chunk)
			seq := db.nextChunkSeq
			db.nextChunkSeq++
			touchedDatastores[row.Key] = ds
			groups[key] = append(groups[key], chunkIngestWork{row: row, seq: seq, ds: ds, chunk: chunk})
		}
	}
	for _, ds := range touchedDatastores {
		if err := db.ensureChunkDatastoreFilesLocked(ds); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
	}
	db.chunkMu.Unlock()

	workerResults := db.processChunkTileBatches(ctx, groups, groupOrder)
	result := &IngestChunkResult{}
	progress := map[DatastoreKey]chunkDatastoreProgress{}
	events := map[DatastoreKey]map[ChunkCoord][]chunkLogEntry{}
	for _, workerResult := range workerResults {
		if workerResult.err != nil {
			return nil, workerResult.err
		}
		result.AcceptedRows += workerResult.result.AcceptedRows
		result.ChangedPixels += workerResult.result.ChangedPixels
		result.UnchangedPixels += workerResult.result.UnchangedPixels
		if workerResult.result.LatestRowSeq > result.LatestRowSeq {
			result.LatestRowSeq = workerResult.result.LatestRowSeq
		}
		for key := range workerResult.touched {
			touched[key] = struct{}{}
		}
		for key, workerProgress := range workerResult.progress {
			if current, ok := progress[key]; !ok || workerProgress.LatestRowSeq > current.LatestRowSeq {
				progress[key] = workerProgress
			}
		}
		for key, chunkEvents := range workerResult.events {
			target := events[key]
			if target == nil {
				target = map[ChunkCoord][]chunkLogEntry{}
				events[key] = target
			}
			for coord, entries := range chunkEvents {
				target[coord] = append(target[coord], entries...)
			}
		}
	}
	db.chunkMu.Lock()
	defer db.chunkMu.Unlock()
	for key, chunkEvents := range events {
		ds := touchedDatastores[key]
		for coord, entries := range chunkEvents {
			ds.events[coord] = append(ds.events[coord], entries...)
		}
	}
	for key, latest := range progress {
		ds := touchedDatastores[key]
		if latest.LatestRowSeq > ds.latestRowSeq {
			ds.latestTick = latest.LatestTick
			ds.latestChunk = ChunkCoord{X: latest.LatestChunkX, Y: latest.LatestChunkY}
			ds.latestRowSeq = latest.LatestRowSeq
		}
	}
	for _, ds := range touchedDatastores {
		if err := db.writeChunkProgressLocked(ds); err != nil {
			return nil, err
		}
	}
	result.DatastoresTouched = len(touched)
	return result, nil
}

func (db *DB) copyChunksTo(ctx context.Context, dst *DB) (*ChunkCopyStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db.closed {
		return nil, errors.New("timeshadedb: source database is closed")
	}
	if dst == nil || dst.closed {
		return nil, errors.New("timeshadedb: destination database is closed")
	}
	if db.format != FormatChunks || dst.format != FormatChunks {
		return nil, errors.New("timeshadedb: chunk copy requires chunk databases")
	}
	if dst.readOnly {
		return nil, errors.New("timeshadedb: destination database opened read-only")
	}
	if db == dst {
		return nil, errors.New("timeshadedb: source and destination databases must differ")
	}

	datastores, err := db.snapshotChunkCopyDatastores(ctx)
	if err != nil {
		return nil, err
	}
	stats := &ChunkCopyStats{Datastores: len(datastores)}
	var batch []ParsedChunkRow
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := dst.IngestChunkRows(ctx, batch)
		if err != nil {
			return err
		}
		stats.RowsCopied += res.AcceptedRows
		stats.ChangedPixels += res.ChangedPixels
		batch = batch[:0]
		return nil
	}
	for _, ds := range datastores {
		coords := make([]ChunkCoord, 0, len(ds.events))
		for coord := range ds.events {
			coords = append(coords, coord)
		}
		sort.Slice(coords, func(i, j int) bool {
			if coords[i].X != coords[j].X {
				return coords[i].X < coords[j].X
			}
			return coords[i].Y < coords[j].Y
		})
		latestPixels := map[ChunkCoord][ChunkPixelCount]uint16{}
		for _, coord := range coords {
			entries := append([]chunkLogEntry(nil), ds.events[coord]...)
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].Seq != entries[j].Seq {
					return entries[i].Seq < entries[j].Seq
				}
				return entries[i].Tick < entries[j].Tick
			})
			var pixels [ChunkPixelCount]uint16
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				for _, change := range entry.Changes {
					if int(change.Pos) >= len(pixels) {
						return nil, fmt.Errorf("timeshadedb: chunk log position out of bounds: %d", change.Pos)
					}
					pixels[change.Pos] = change.Color
				}
				batch = append(batch, ParsedChunkRow{Key: ds.key, Tick: entry.Tick, Chunk: coord, Pixels: pixels})
				if len(batch) >= chunkCopyBatchRows {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
			latestPixels[coord] = pixels
			stats.Chunks++
		}
		if ds.latestRowSeq > 0 {
			if pixels, ok := latestPixels[ds.latestChunk]; ok {
				batch = append(batch, ParsedChunkRow{Key: ds.key, Tick: ds.latestTick, Chunk: ds.latestChunk, Pixels: pixels})
				stats.ProgressRows++
				if len(batch) >= chunkCopyBatchRows {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return stats, nil
}

func (db *DB) snapshotChunkCopyDatastores(ctx context.Context) ([]chunkCopyDatastore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if !loaded {
		db.chunkMu.Lock()
		if err := db.loadChunksLocked(); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
		db.chunkMu.Unlock()
	}
	db.chunkMu.RLock()
	defer db.chunkMu.RUnlock()
	keys := make([]DatastoreKey, 0, len(db.chunkDatastores))
	for key := range db.chunkDatastores {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].SavefileUUID != keys[j].SavefileUUID {
			return keys[i].SavefileUUID < keys[j].SavefileUUID
		}
		if keys[i].Force != keys[j].Force {
			return keys[i].Force < keys[j].Force
		}
		return keys[i].Surface < keys[j].Surface
	})
	out := make([]chunkCopyDatastore, 0, len(keys))
	for _, key := range keys {
		ds := db.chunkDatastores[key]
		copyDS := chunkCopyDatastore{
			key:          key,
			latestTick:   ds.latestTick,
			latestChunk:  ds.latestChunk,
			latestRowSeq: ds.latestRowSeq,
			events:       make(map[ChunkCoord][]chunkLogEntry, len(ds.events)),
		}
		for coord, entries := range ds.events {
			copied := make([]chunkLogEntry, len(entries))
			for i, entry := range entries {
				copied[i] = entry
				copied[i].Changes = append([]chunkPixelChange(nil), entry.Changes...)
			}
			copyDS.events[coord] = copied
		}
		out = append(out, copyDS)
	}
	return out, nil
}

func (db *DB) processChunkTileBatches(ctx context.Context, groups map[chunkTileBatchKey][]chunkIngestWork, order []chunkTileBatchKey) []chunkTileBatchResult {
	if len(order) == 0 {
		return nil
	}
	workerCount := runtime.GOMAXPROCS(0)
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(order) {
		workerCount = len(order)
	}
	jobs := make(chan chunkTileBatchKey)
	results := make(chan chunkTileBatchResult, len(order))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer wg.Done()
			for key := range jobs {
				results <- db.processChunkTileBatch(ctx, groups[key])
			}
		}()
	}
	for _, key := range order {
		jobs <- key
	}
	close(jobs)
	wg.Wait()
	close(results)
	out := make([]chunkTileBatchResult, 0, len(order))
	for result := range results {
		out = append(out, result)
	}
	return out
}

func (db *DB) acquireChunkTileLocks(keys []chunkTileBatchKey) func() {
	ordered := append([]chunkTileBatchKey(nil), keys...)
	sort.Slice(ordered, func(i, j int) bool {
		return compareChunkTileBatchKey(ordered[i], ordered[j]) < 0
	})
	locks := make([]*sync.Mutex, 0, len(ordered))
	for _, key := range ordered {
		lock := db.chunkTileLock(key)
		lock.Lock()
		locks = append(locks, lock)
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

func (db *DB) chunkTileLock(key chunkTileBatchKey) *sync.Mutex {
	v, _ := db.chunkTileLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func compareChunkTileBatchKey(a, b chunkTileBatchKey) int {
	if a.key.SavefileUUID != b.key.SavefileUUID {
		if a.key.SavefileUUID < b.key.SavefileUUID {
			return -1
		}
		return 1
	}
	if a.key.Surface != b.key.Surface {
		if a.key.Surface < b.key.Surface {
			return -1
		}
		return 1
	}
	if a.key.Force != b.key.Force {
		if a.key.Force < b.key.Force {
			return -1
		}
		return 1
	}
	if a.tileX != b.tileX {
		if a.tileX < b.tileX {
			return -1
		}
		return 1
	}
	if a.tileY != b.tileY {
		if a.tileY < b.tileY {
			return -1
		}
		return 1
	}
	return 0
}

func (db *DB) processChunkTileBatch(ctx context.Context, batch []chunkIngestWork) chunkTileBatchResult {
	result := chunkTileBatchResult{
		touched:  map[DatastoreKey]struct{}{},
		progress: map[DatastoreKey]chunkDatastoreProgress{},
		events:   map[DatastoreKey]map[ChunkCoord][]chunkLogEntry{},
	}
	writes := make([]chunkWrite, 0, len(batch))
	lastPayloads := map[ChunkCoord]string{}
	for _, work := range batch {
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		row := work.row
		ds := work.ds
		chunk := work.chunk
		seq := work.seq
		pixels := row.Pixels
		if row.HasPayload {
			if lastPayload, ok := lastPayloads[row.Chunk]; ok && chunk.seen && row.PayloadHex == lastPayload {
				chunk.latestTick = row.Tick
				chunk.latestRowSeq = seq
				result.recordChunkProgress(row, seq, 0, ChunkPixelCount)
				continue
			}
			lastPayloads[row.Chunk] = row.PayloadHex
			decoded, err := decodeRGB565Hex(row.PayloadHex)
			if err != nil {
				result.err = err
				return result
			}
			pixels = decoded
		}
		wasSeen := chunk.seen
		changes := make([]chunkPixelChange, 0, ChunkPixelCount)
		for i, color := range pixels {
			if !chunk.seen || chunk.pixels[i] != color {
				chunk.pixels[i] = color
				changes = append(changes, chunkPixelChange{Pos: uint16(i), Color: color})
			}
		}
		chunk.seen = true
		chunk.latestTick = row.Tick
		chunk.latestRowSeq = seq
		entry := chunkLogEntry{Seq: seq, Tick: row.Tick, Key: row.Key, Chunk: row.Chunk, Changes: changes}
		if !wasSeen || len(changes) > 0 {
			write := chunkWrite{
				ds:       ds,
				coord:    row.Chunk,
				chunk:    chunk,
				entry:    entry,
				snapshot: !wasSeen || db.shouldSnapshotChunk(chunk, entry),
			}
			if write.snapshot {
				write.snapshotPixels = chunk.pixels
			}
			writes = append(writes, write)
			chunkEvents := result.events[row.Key]
			if chunkEvents == nil {
				chunkEvents = map[ChunkCoord][]chunkLogEntry{}
				result.events[row.Key] = chunkEvents
			}
			chunkEvents[row.Chunk] = append(chunkEvents[row.Chunk], entry)
		}
		result.recordChunkProgress(row, seq, uint64(len(changes)), uint64(ChunkPixelCount-len(changes)))
	}
	if err := db.writeChunkBatchLocked(writes); err != nil {
		result.err = err
		return result
	}
	result.result.DatastoresTouched = len(result.touched)
	return result
}

func (r *chunkTileBatchResult) recordChunkProgress(row ParsedChunkRow, seq uint64, changed, unchanged uint64) {
	r.touched[row.Key] = struct{}{}
	r.result.AcceptedRows++
	r.result.ChangedPixels += changed
	r.result.UnchangedPixels += unchanged
	if seq > r.result.LatestRowSeq {
		r.result.LatestRowSeq = seq
	}
	current, ok := r.progress[row.Key]
	if !ok || seq > current.LatestRowSeq {
		r.progress[row.Key] = chunkDatastoreProgress{
			LatestTick:   row.Tick,
			LatestChunkX: row.Chunk.X,
			LatestChunkY: row.Chunk.Y,
			LatestRowSeq: seq,
		}
	}
}

func (db *DB) shouldSnapshotChunk(chunk *factorioChunkState, entry chunkLogEntry) bool {
	if !chunk.seen || !chunk.tileBacked || len(entry.Changes) == 0 {
		return false
	}
	if chunk.deltaEventsSinceSnap >= chunkSnapshotMaxDeltaEvents {
		return true
	}
	if chunk.deltaFramesSinceSnap >= chunkSnapshotMaxDeltaFrames {
		return true
	}
	if chunk.lastSnapshotBytes > 0 && chunk.deltaBytesSinceSnapshot >= uint64(chunk.lastSnapshotBytes)*2 {
		return true
	}
	return false
}

func (db *DB) chunkAt(ctx context.Context, opts ChunkAtOptions) (*ChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDatastoreKey(opts.Key); err != nil {
		return nil, err
	}
	chunks, err := db.chunksAt(ctx, opts.Key, opts.Tick, []ChunkCoord{opts.Chunk})
	if err != nil {
		return nil, err
	}
	chunk := chunks[opts.Chunk]
	if chunk == nil {
		return nil, fmt.Errorf("timeshadedb: chunk has no data at tick %d: %d,%d", opts.Tick, opts.Chunk.X, opts.Chunk.Y)
	}
	return chunk, nil
}

func (db *DB) chunksAt(ctx context.Context, key DatastoreKey, tick uint64, coords []ChunkCoord) (map[ChunkCoord]*ChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDatastoreKey(key); err != nil {
		return nil, err
	}
	if len(coords) == 0 {
		return map[ChunkCoord]*ChunkResult{}, nil
	}
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if !loaded {
		db.chunkMu.Lock()
		if err := db.loadChunksLocked(); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
		db.chunkMu.Unlock()
	}

	groups := map[chunkTileCoord][]ChunkCoord{}
	for _, coord := range coords {
		tile := chunkTileCoordForChunk(coord)
		groups[tile] = append(groups[tile], coord)
	}
	results := map[ChunkCoord]*ChunkResult{}
	tiles := make([]chunkTileCoord, 0, len(groups))
	for tile := range groups {
		tiles = append(tiles, tile)
	}
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].X != tiles[j].X {
			return tiles[i].X < tiles[j].X
		}
		return tiles[i].Y < tiles[j].Y
	})
	for _, tile := range tiles {
		tileResults, err := db.chunksAtInTile(ctx, key, tick, tile, groups[tile])
		if err != nil {
			return nil, err
		}
		for coord, result := range tileResults {
			results[coord] = result
		}
	}
	return results, nil
}

func (db *DB) chunksAtInTile(ctx context.Context, key DatastoreKey, tick uint64, tile chunkTileCoord, coords []ChunkCoord) (map[ChunkCoord]*ChunkResult, error) {
	lock := db.chunkTileLock(chunkTileBatchKey{key: key, tileX: tile.X, tileY: tile.Y})
	lock.Lock()
	defer lock.Unlock()

	db.chunkMu.RLock()
	ds := db.chunkDatastores[key]
	if ds == nil {
		db.chunkMu.RUnlock()
		return nil, fmt.Errorf("timeshadedb: datastore not found: %s/%s/%s", key.SavefileUUID, key.Surface, key.Force)
	}
	results := map[ChunkCoord]*ChunkResult{}
	plans := make([]chunkReadPlan, 0, len(coords))
	eventFallbacks := map[ChunkCoord][]chunkLogEntry{}
	for _, coord := range coords {
		chunk := ds.chunks[coord]
		if chunk == nil || !chunk.seen {
			continue
		}
		if tick >= chunk.latestTick {
			pixels := make([]uint16, ChunkPixelCount)
			copy(pixels, chunk.pixels[:])
			results[coord] = &ChunkResult{
				Key:          key,
				Chunk:        coord,
				Tick:         tick,
				Width:        ChunkSize,
				Height:       ChunkSize,
				Pixels:       pixels,
				SnapshotTick: chunk.latestTick,
			}
			continue
		}
		idx := cloneChunkIndex(chunk.index)
		if len(idx.snapshots) == 0 {
			eventFallbacks[coord] = append([]chunkLogEntry(nil), ds.events[coord]...)
			continue
		}
		path := chunkTileDataPath(db.path, key, tile)
		if !chunk.tileBacked {
			path = chunkDataPath(db.path, key, coord)
		}
		plans = append(plans, chunkReadPlan{coord: coord, path: path, tile: tile, idx: idx})
		eventFallbacks[coord] = append([]chunkLogEntry(nil), ds.events[coord]...)
	}
	db.chunkMu.RUnlock()

	if len(plans) == 0 {
		for coord, events := range eventFallbacks {
			if result := chunkResultFromEvents(key, tick, coord, events); result != nil {
				results[coord] = result
			}
		}
		return results, nil
	}
	replayed, err := db.replayChunkReadPlans(ctx, key, tick, plans)
	if err != nil {
		return nil, err
	}
	for coord, result := range replayed {
		results[coord] = result
	}
	for coord, events := range eventFallbacks {
		if results[coord] != nil {
			continue
		}
		if result := chunkResultFromEvents(key, tick, coord, events); result != nil {
			results[coord] = result
		}
	}
	return results, nil
}

func (db *DB) replayChunkReadPlans(ctx context.Context, key DatastoreKey, tick uint64, plans []chunkReadPlan) (map[ChunkCoord]*ChunkResult, error) {
	states := map[ChunkCoord]*chunkReplayState{}
	reads := make([]chunkFrameRead, 0)
	for _, plan := range plans {
		snapIdx := latestChunkSnapshotAtOrBefore(plan.idx.snapshots, tick)
		if snapIdx < 0 {
			continue
		}
		snap := plan.idx.snapshots[snapIdx]
		states[plan.coord] = &chunkReplayState{snapshotTick: snap.Tick}
		reads = append(reads, chunkFrameRead{
			coord:  plan.coord,
			path:   plan.path,
			tile:   plan.tile,
			offset: snap.FrameOffset,
			kind:   frameKindSnapshot,
		})
		end := uint32(len(plan.idx.deltas))
		if snapIdx+1 < len(plan.idx.snapshots) {
			end = plan.idx.snapshots[snapIdx+1].FirstDeltaFrameIdx
		}
		for di := snap.FirstDeltaFrameIdx; di < end; di++ {
			delta := plan.idx.deltas[di]
			if delta.MinTick > tick {
				break
			}
			reads = append(reads, chunkFrameRead{
				coord:  plan.coord,
				path:   plan.path,
				tile:   plan.tile,
				offset: delta.FrameOffset,
				kind:   frameKindDelta,
			})
		}
	}
	sort.Slice(reads, func(i, j int) bool {
		if reads[i].path != reads[j].path {
			return reads[i].path < reads[j].path
		}
		return reads[i].offset < reads[j].offset
	})

	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for _, read := range reads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := files[read.path]
		if f == nil {
			var err error
			f, err = os.Open(read.path)
			if err != nil {
				return nil, err
			}
			switch filepath.Ext(read.path) {
			case ".tdat":
				if err := readChunkTileDataHeader(f, read.tile); err != nil {
					_ = f.Close()
					return nil, err
				}
			case ".cdat":
				if err := readChunkDataHeader(f, read.coord); err != nil {
					_ = f.Close()
					return nil, err
				}
			}
			files[read.path] = f
		}
		info, err := readChunkFrame(f, read.offset)
		if err != nil {
			return nil, err
		}
		if info.kind != read.kind {
			return nil, fmt.Errorf("timeshadedb: chunk index kind mismatch: index %d frame %d", read.kind, info.kind)
		}
		raw, err := decompressZstd(info.payload, info.rawLen)
		if err != nil {
			return nil, err
		}
		if crc32.ChecksumIEEE(raw) != info.checksum {
			return nil, errors.New("timeshadedb: chunk frame checksum mismatch")
		}
		state := states[read.coord]
		if state == nil {
			continue
		}
		switch read.kind {
		case frameKindSnapshot:
			pixels, err := decodeChunkSnapshot(raw)
			if err != nil {
				return nil, err
			}
			state.pixels = pixels
			state.seen = true
			state.snapshotTick = info.tick
		case frameKindDelta:
			deltaTick, changes, err := decodeChunkDeltaPayload(raw)
			if err != nil {
				return nil, err
			}
			if deltaTick != info.tick {
				return nil, fmt.Errorf("timeshadedb: chunk delta tick mismatch: payload %d frame %d", deltaTick, info.tick)
			}
			if info.tick > tick {
				continue
			}
			for _, change := range changes {
				state.pixels[change.Pos] = change.Color
			}
			state.replayed += len(changes)
		}
	}

	results := map[ChunkCoord]*ChunkResult{}
	for coord, state := range states {
		if !state.seen {
			continue
		}
		pixels := make([]uint16, ChunkPixelCount)
		copy(pixels, state.pixels[:])
		results[coord] = &ChunkResult{
			Key:          key,
			Chunk:        coord,
			Tick:         tick,
			Width:        ChunkSize,
			Height:       ChunkSize,
			Pixels:       pixels,
			SnapshotTick: state.snapshotTick,
			Replayed:     state.replayed,
		}
	}
	return results, nil
}

func latestChunkSnapshotAtOrBefore(snaps []chunkSnapshotRecord, tick uint64) int {
	i := sort.Search(len(snaps), func(i int) bool {
		return snaps[i].Tick > tick
	})
	return i - 1
}

func cloneChunkIndex(idx chunkIndex) chunkIndex {
	return chunkIndex{
		snapshots: append([]chunkSnapshotRecord(nil), idx.snapshots...),
		deltas:    append([]chunkDeltaRecord(nil), idx.deltas...),
	}
}

func chunkResultFromEvents(key DatastoreKey, tick uint64, coord ChunkCoord, events []chunkLogEntry) *ChunkResult {
	var pixels [ChunkPixelCount]uint16
	var seen bool
	var replayed int
	var snapshotTick uint64
	for _, entry := range events {
		if entry.Tick > tick {
			break
		}
		seen = true
		snapshotTick = entry.Tick
		for _, change := range entry.Changes {
			if int(change.Pos) >= len(pixels) {
				return nil
			}
			pixels[change.Pos] = change.Color
			replayed++
		}
	}
	if !seen {
		return nil
	}
	out := make([]uint16, ChunkPixelCount)
	copy(out, pixels[:])
	return &ChunkResult{
		Key:          key,
		Chunk:        coord,
		Tick:         tick,
		Width:        ChunkSize,
		Height:       ChunkSize,
		Pixels:       out,
		SnapshotTick: snapshotTick,
		Replayed:     replayed,
	}
}

func (db *DB) ingestMetadata(savefileUUID string) (*IngestMetadata, error) {
	savefileUUID = strings.TrimSpace(savefileUUID)
	if savefileUUID == "" {
		return nil, errors.New("timeshadedb: savefile UUID is required")
	}
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if !loaded {
		db.chunkMu.Lock()
		if err := db.loadChunksLocked(); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
		db.chunkMu.Unlock()
	}
	db.chunkMu.RLock()
	defer db.chunkMu.RUnlock()
	meta := &IngestMetadata{SavefileUUID: savefileUUID}
	for key, ds := range db.chunkDatastores {
		if key.SavefileUUID != savefileUUID {
			continue
		}
		meta.Datastores = append(meta.Datastores, IngestDatastoreMetadata{
			Surface:      key.Surface,
			Force:        key.Force,
			LatestTick:   ds.latestTick,
			LatestChunkX: ds.latestChunk.X,
			LatestChunkY: ds.latestChunk.Y,
			LatestRowSeq: ds.latestRowSeq,
		})
	}
	sort.Slice(meta.Datastores, func(i, j int) bool {
		if meta.Datastores[i].Surface != meta.Datastores[j].Surface {
			return meta.Datastores[i].Surface < meta.Datastores[j].Surface
		}
		return meta.Datastores[i].Force < meta.Datastores[j].Force
	})
	return meta, nil
}

func (db *DB) chunkSaveCatalog() (*ChunkSaveCatalog, error) {
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if !loaded {
		db.chunkMu.Lock()
		if err := db.loadChunksLocked(); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
		db.chunkMu.Unlock()
	}

	db.chunkMu.RLock()
	defer db.chunkMu.RUnlock()
	saveMap := map[string]*ChunkSaveSummary{}
	for key, ds := range db.chunkDatastores {
		save := saveMap[key.SavefileUUID]
		if save == nil {
			save = &ChunkSaveSummary{SavefileUUID: key.SavefileUUID}
			saveMap[key.SavefileUUID] = save
		}
		save.Datastores = append(save.Datastores, summarizeChunkDatastore(ds))
	}
	catalog := &ChunkSaveCatalog{Saves: make([]ChunkSaveSummary, 0, len(saveMap))}
	for _, save := range saveMap {
		forceSet := map[string]struct{}{}
		surfaceSet := map[string]struct{}{}
		for _, ds := range save.Datastores {
			forceSet[ds.Force] = struct{}{}
			surfaceSet[ds.Surface] = struct{}{}
		}
		save.Forces = sortedStringKeys(forceSet)
		save.Surfaces = sortedStringKeys(surfaceSet)
		sort.Slice(save.Datastores, func(i, j int) bool {
			if save.Datastores[i].Force != save.Datastores[j].Force {
				return save.Datastores[i].Force < save.Datastores[j].Force
			}
			return save.Datastores[i].Surface < save.Datastores[j].Surface
		})
		catalog.Saves = append(catalog.Saves, *save)
	}
	sort.Slice(catalog.Saves, func(i, j int) bool {
		return catalog.Saves[i].SavefileUUID < catalog.Saves[j].SavefileUUID
	})
	return catalog, nil
}

func (db *DB) chunkStats() (*ChunkStats, error) {
	if db.format != FormatChunks {
		return nil, errors.New("timeshadedb: chunk stats require a chunk database")
	}
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if !loaded {
		db.chunkMu.Lock()
		if err := db.loadChunksLocked(); err != nil {
			db.chunkMu.Unlock()
			return nil, err
		}
		db.chunkMu.Unlock()
	}

	db.chunkMu.RLock()
	defer db.chunkMu.RUnlock()
	stats := &ChunkStats{Format: FormatChunks, DatastoreCount: len(db.chunkDatastores)}
	keys := make([]DatastoreKey, 0, len(db.chunkDatastores))
	for key := range db.chunkDatastores {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return compareDatastoreKeys(keys[i], keys[j]) < 0
	})
	for _, key := range keys {
		dsStats := summarizeChunkDatastoreStats(db.chunkDatastores[key])
		stats.ChunkCount += dsStats.ChunkCount
		stats.TileShardCount += dsStats.TileShardCount
		stats.SnapshotCount += dsStats.SnapshotCount
		stats.DeltaFrameCount += dsStats.DeltaFrameCount
		stats.CompressedBytes += dsStats.CompressedBytes
		stats.StoredPixelEvents += dsStats.StoredPixelEvents
		if dsStats.MaxReplayEventsBetweenSnaps > stats.MaxReplayEventsBetweenSnaps {
			stats.MaxReplayEventsBetweenSnaps = dsStats.MaxReplayEventsBetweenSnaps
		}
		stats.Datastores = append(stats.Datastores, dsStats)
	}
	if stats.SnapshotCount > 0 {
		var snapshotBytes uint64
		for _, ds := range stats.Datastores {
			snapshotBytes += ds.AverageSnapshotBytes * ds.SnapshotCount
		}
		stats.AverageSnapshotBytes = snapshotBytes / stats.SnapshotCount
	}
	if stats.DeltaFrameCount > 0 {
		var deltaBytes uint64
		for _, ds := range stats.Datastores {
			deltaBytes += ds.AverageDeltaFrameBytes * ds.DeltaFrameCount
		}
		stats.AverageDeltaFrameBytes = deltaBytes / stats.DeltaFrameCount
	}
	return stats, nil
}

func summarizeChunkDatastoreStats(ds *chunkDatastore) ChunkDatastoreStats {
	out := ChunkDatastoreStats{
		SavefileUUID: ds.key.SavefileUUID,
		Surface:      ds.key.Surface,
		Force:        ds.key.Force,
		LatestTick:   ds.latestTick,
		LatestRowSeq: ds.latestRowSeq,
	}
	first := true
	tileSet := map[chunkTileCoord]struct{}{}
	var snapshotBytes uint64
	var deltaBytes uint64
	for coord, chunk := range ds.chunks {
		tile := chunkTileCoordForChunk(coord)
		tileSet[tile] = struct{}{}
		if first {
			out.MinChunkX = coord.X
			out.MaxChunkX = coord.X
			out.MinChunkY = coord.Y
			out.MaxChunkY = coord.Y
			out.MinTileX = tile.X
			out.MaxTileX = tile.X
			out.MinTileY = tile.Y
			out.MaxTileY = tile.Y
			first = false
		} else {
			if coord.X < out.MinChunkX {
				out.MinChunkX = coord.X
			}
			if coord.X > out.MaxChunkX {
				out.MaxChunkX = coord.X
			}
			if coord.Y < out.MinChunkY {
				out.MinChunkY = coord.Y
			}
			if coord.Y > out.MaxChunkY {
				out.MaxChunkY = coord.Y
			}
			if tile.X < out.MinTileX {
				out.MinTileX = tile.X
			}
			if tile.X > out.MaxTileX {
				out.MaxTileX = tile.X
			}
			if tile.Y < out.MinTileY {
				out.MinTileY = tile.Y
			}
			if tile.Y > out.MaxTileY {
				out.MaxTileY = tile.Y
			}
		}
		out.ChunkCount++
		out.SnapshotCount += uint64(len(chunk.index.snapshots))
		out.DeltaFrameCount += uint64(len(chunk.index.deltas))
		for _, snap := range chunk.index.snapshots {
			snapshotBytes += uint64(snap.CompressedLen)
			out.StoredPixelEvents += ChunkPixelCount
		}
		for _, delta := range chunk.index.deltas {
			deltaBytes += uint64(delta.CompressedLen)
			out.StoredPixelEvents += uint64(delta.EventCount)
		}
		if replay := maxChunkReplayEvents(chunk.index); replay > out.MaxReplayEventsBetweenSnaps {
			out.MaxReplayEventsBetweenSnaps = replay
		}
	}
	out.TileShardCount = uint64(len(tileSet))
	out.CompressedBytes = snapshotBytes + deltaBytes
	if out.SnapshotCount > 0 {
		out.AverageSnapshotBytes = snapshotBytes / out.SnapshotCount
	}
	if out.DeltaFrameCount > 0 {
		out.AverageDeltaFrameBytes = deltaBytes / out.DeltaFrameCount
	}
	return out
}

func maxChunkReplayEvents(idx chunkIndex) uint32 {
	var maxReplay uint32
	for i, snap := range idx.snapshots {
		end := uint32(len(idx.deltas))
		if i+1 < len(idx.snapshots) {
			end = idx.snapshots[i+1].FirstDeltaFrameIdx
		}
		var replay uint32
		for di := snap.FirstDeltaFrameIdx; di < end; di++ {
			replay += idx.deltas[di].EventCount
		}
		if replay > maxReplay {
			maxReplay = replay
		}
	}
	return maxReplay
}

func compareDatastoreKeys(a, b DatastoreKey) int {
	if a.SavefileUUID != b.SavefileUUID {
		if a.SavefileUUID < b.SavefileUUID {
			return -1
		}
		return 1
	}
	if a.Force != b.Force {
		if a.Force < b.Force {
			return -1
		}
		return 1
	}
	if a.Surface != b.Surface {
		if a.Surface < b.Surface {
			return -1
		}
		return 1
	}
	return 0
}

func summarizeChunkDatastore(ds *chunkDatastore) ChunkDatastoreSummary {
	out := ChunkDatastoreSummary{
		Surface:      ds.key.Surface,
		Force:        ds.key.Force,
		LatestTick:   ds.latestTick,
		LatestRowSeq: ds.latestRowSeq,
	}
	first := true
	for coord := range ds.chunks {
		if first {
			out.MinChunkX = coord.X
			out.MaxChunkX = coord.X
			out.MinChunkY = coord.Y
			out.MaxChunkY = coord.Y
			first = false
		} else {
			if coord.X < out.MinChunkX {
				out.MinChunkX = coord.X
			}
			if coord.X > out.MaxChunkX {
				out.MaxChunkX = coord.X
			}
			if coord.Y < out.MinChunkY {
				out.MinChunkY = coord.Y
			}
			if coord.Y > out.MaxChunkY {
				out.MaxChunkY = coord.Y
			}
		}
		out.ChunkCount++
	}
	if out.ChunkCount > 0 {
		chunksPerTile := TileSize / ChunkSize
		out.MinTileX = floorDiv32(out.MinChunkX, chunksPerTile)
		out.MaxTileX = floorDiv32(out.MaxChunkX, chunksPerTile)
		out.MinTileY = floorDiv32(out.MinChunkY, chunksPerTile)
		out.MaxTileY = floorDiv32(out.MaxChunkY, chunksPerTile)
	}
	return out
}

func sortedStringKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ParseChunkTSVRow(savefileUUID, line string) (ParsedChunkRow, error) {
	return parseChunkTSVRow(savefileUUID, line, true)
}

func parseChunkTSVRow(savefileUUID, line string, decodePayload bool) (ParsedChunkRow, error) {
	savefileUUID = strings.TrimSpace(savefileUUID)
	line = strings.TrimRight(line, "\r\n")
	fields := strings.Split(line, "\t")
	if len(fields) != 5 && len(fields) != 6 {
		return ParsedChunkRow{}, fmt.Errorf("timeshadedb: expected 5 or 6 TSV fields, got %d", len(fields))
	}
	tick, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 64)
	if err != nil {
		return ParsedChunkRow{}, fmt.Errorf("timeshadedb: invalid game tick: %w", err)
	}
	surface := strings.TrimSpace(fields[1])
	var chunkX, chunkY int64
	var force, colorData string
	if len(fields) == 5 {
		parts := strings.Split(strings.TrimSpace(fields[2]), ",")
		if len(parts) != 2 {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: expected chunk coordinate x,y, got %q", fields[2])
		}
		chunkX, err = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32)
		if err != nil {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: invalid chunk x: %w", err)
		}
		chunkY, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: invalid chunk y: %w", err)
		}
		force = strings.TrimSpace(fields[3])
		colorData = strings.TrimSpace(fields[4])
	} else {
		chunkX, err = strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 32)
		if err != nil {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: invalid chunk x: %w", err)
		}
		chunkY, err = strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 32)
		if err != nil {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: invalid chunk y: %w", err)
		}
		force = strings.TrimSpace(fields[4])
		colorData = strings.TrimSpace(fields[5])
	}
	key := DatastoreKey{SavefileUUID: savefileUUID, Surface: surface, Force: force}
	if err := validateDatastoreKey(key); err != nil {
		return ParsedChunkRow{}, err
	}
	row := ParsedChunkRow{
		Key:   key,
		Tick:  tick,
		Chunk: ChunkCoord{X: int32(chunkX), Y: int32(chunkY)},
	}
	if decodePayload {
		pixels, err := decodeRGB565Hex(colorData)
		if err != nil {
			return ParsedChunkRow{}, err
		}
		row.Pixels = pixels
	} else {
		if colorData != "" && len(colorData) != chunkPayloadHex {
			return ParsedChunkRow{}, fmt.Errorf("timeshadedb: RGB565 hex payload has %d chars, expected %d", len(colorData), chunkPayloadHex)
		}
		row.PayloadHex = colorData
		row.HasPayload = true
	}
	return row, nil
}

func ParseChunkTSV(savefileUUID string, body *bufio.Scanner) ([]ParsedChunkRow, error) {
	var rows []ParsedChunkRow
	for lineNo := 1; body.Scan(); lineNo++ {
		line := body.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		row, err := parseChunkTSVRow(savefileUUID, line, false)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		rows = append(rows, row)
	}
	if err := body.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func decodeRGB565Hex(s string) ([ChunkPixelCount]uint16, error) {
	var pixels [ChunkPixelCount]uint16
	s = strings.TrimSpace(s)
	if s == "" {
		return pixels, nil
	}
	if len(s) != chunkPayloadHex {
		return pixels, fmt.Errorf("timeshadedb: RGB565 hex payload has %d chars, expected %d", len(s), chunkPayloadHex)
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return pixels, fmt.Errorf("timeshadedb: invalid RGB565 hex payload: %w", err)
	}
	if len(raw) != chunkPayloadBytes {
		return pixels, fmt.Errorf("timeshadedb: RGB565 payload has %d bytes, expected %d", len(raw), chunkPayloadBytes)
	}
	for i := 0; i < ChunkPixelCount; i++ {
		pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return pixels, nil
}

func chunkTileKey(key DatastoreKey, coord ChunkCoord) chunkTileBatchKey {
	return chunkTileBatchKey{
		key:   key,
		tileX: floorDiv32(coord.X, TileSize/ChunkSize),
		tileY: floorDiv32(coord.Y, TileSize/ChunkSize),
	}
}

func floorDiv32(v int32, d int) int32 {
	q := v / int32(d)
	if v < 0 && v%int32(d) != 0 {
		q--
	}
	return q
}

func validateDatastoreKey(key DatastoreKey) error {
	if strings.TrimSpace(key.SavefileUUID) == "" {
		return errors.New("timeshadedb: savefile UUID is required")
	}
	if strings.TrimSpace(key.Surface) == "" {
		return errors.New("timeshadedb: surface is required")
	}
	if strings.TrimSpace(key.Force) == "" {
		return errors.New("timeshadedb: force is required")
	}
	return nil
}

func (db *DB) loadChunksLocked() error {
	if db.chunkLoaded {
		return nil
	}
	db.chunkDatastores = map[DatastoreKey]*chunkDatastore{}
	db.nextChunkSeq = 1
	if _, err := db.loadChunkDataFilesLocked(); err != nil {
		return err
	}
	db.chunkLoaded = true
	return nil
}

func (db *DB) writeChunkFrameLocked(ds *chunkDatastore, coord ChunkCoord, chunk *factorioChunkState, entry chunkLogEntry, snapshot bool) error {
	write := chunkWrite{ds: ds, coord: coord, chunk: chunk, entry: entry, snapshot: snapshot}
	if snapshot {
		write.snapshotPixels = chunk.pixels
	}
	return db.writeChunkBatchLocked([]chunkWrite{write})
}

func (db *DB) writeChunkBatchLocked(writes []chunkWrite) error {
	type groupKey struct {
		key   DatastoreKey
		tileX int32
		tileY int32
	}
	groups := map[groupKey][]chunkWrite{}
	order := make([]groupKey, 0)
	for _, write := range writes {
		tile := chunkTileCoordForChunk(write.coord)
		k := groupKey{key: write.ds.key, tileX: tile.X, tileY: tile.Y}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], write)
	}
	for _, k := range order {
		group := groups[k]
		if len(group) == 0 {
			continue
		}
		if err := db.writeChunkTileGroupLocked(group); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) writeChunkTileGroupLocked(writes []chunkWrite) error {
	first := writes[0]
	ds := first.ds
	tile := chunkTileCoordForChunk(first.coord)
	if err := db.ensureChunkDatastoreFilesLocked(ds); err != nil {
		return err
	}
	dataPath := chunkTileDataPath(db.path, ds.key, tile)
	newFile := false
	if _, err := os.Stat(dataPath); errors.Is(err, os.ErrNotExist) {
		newFile = true
	} else if err != nil {
		return err
	}
	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if newFile {
		if err := writeChunkTileDataHeader(f, tile); err != nil {
			return err
		}
	}
	for _, write := range writes {
		entry := write.entry
		chunk := write.chunk
		var kind uint8
		var raw []byte
		writeSnapshot := write.snapshot || !chunk.tileBacked
		if writeSnapshot {
			kind = frameKindSnapshot
			snapshotPixels := write.snapshotPixels
			if !write.snapshot {
				snapshotPixels = chunk.pixels
			}
			raw = encodeChunkSnapshot(snapshotPixels)
		} else {
			kind = frameKindDelta
			raw = encodeChunkDeltaPayload(entry.Tick, entry.Changes)
		}
		offset, compLen, checksum, err := db.writeChunkFrame(f, kind, entry.Tick, uint32(len(entry.Changes)), raw)
		if err != nil {
			return err
		}
		if writeSnapshot {
			if !chunk.tileBacked {
				chunk.index = chunkIndex{}
			}
			chunk.index.snapshots = append(chunk.index.snapshots, chunkSnapshotRecord{
				Tick:               entry.Tick,
				FrameOffset:        offset,
				FrameHeaderLen:     chunkFrameHeaderLen,
				CompressedLen:      compLen,
				RawLen:             uint32(len(raw)),
				FirstDeltaFrameIdx: uint32(len(chunk.index.deltas)),
				EventSeq:           entry.Seq,
				Checksum:           checksum,
			})
			chunk.tileBacked = true
			chunk.lastSnapshotBytes = compLen
			chunk.deltaFramesSinceSnap = 0
			chunk.deltaEventsSinceSnap = 0
			chunk.deltaBytesSinceSnapshot = 0
		} else {
			chunk.index.deltas = append(chunk.index.deltas, chunkDeltaRecord{
				MinTick:        entry.Tick,
				MaxTick:        entry.Tick,
				FrameOffset:    offset,
				FrameHeaderLen: chunkFrameHeaderLen,
				CompressedLen:  compLen,
				RawLen:         uint32(len(raw)),
				EventCount:     uint32(len(entry.Changes)),
				FirstEventSeq:  entry.Seq,
				LastEventSeq:   entry.Seq,
				Checksum:       checksum,
			})
			chunk.deltaFramesSinceSnap++
			chunk.deltaEventsSinceSnap += uint32(len(entry.Changes))
			chunk.deltaBytesSinceSnapshot += uint64(compLen)
		}
	}
	return writeChunkTileIndex(db.path, ds.key, tile, chunkIndexesForTile(ds, tile))
}

func (db *DB) ensureChunkDatastoreFilesLocked(ds *chunkDatastore) error {
	dsDir := chunkDatastorePath(db.path, ds.key)
	if err := os.MkdirAll(filepath.Join(dsDir, "tiles"), 0o755); err != nil {
		return err
	}
	ds.dirsReady = true
	if !ds.metadataWritten {
		if err := writeChunkMetadata(dsDir, ds.key); err != nil {
			return err
		}
		ds.metadataWritten = true
	}
	return nil
}

func (db *DB) writeChunkProgressLocked(ds *chunkDatastore) error {
	if err := db.ensureChunkDatastoreFilesLocked(ds); err != nil {
		return err
	}
	dsDir := chunkDatastorePath(db.path, ds.key)
	progress := chunkDatastoreProgress{
		LatestTick:   ds.latestTick,
		LatestChunkX: ds.latestChunk.X,
		LatestChunkY: ds.latestChunk.Y,
		LatestRowSeq: ds.latestRowSeq,
	}
	return writeChunkProgress(dsDir, progress)
}

func (db *DB) writeChunkFrame(f *os.File, kind uint8, tick uint64, eventCount uint32, raw []byte) (uint64, uint32, uint32, error) {
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, 0, err
	}
	payload, err := db.compressChunkPayload(raw)
	if err != nil {
		return 0, 0, 0, err
	}
	checksum := crc32.ChecksumIEEE(raw)
	var hdr bytes.Buffer
	hdr.WriteString("CFRM")
	hdr.WriteByte(kind)
	for _, v := range []any{tick, tick, eventCount, uint32(len(raw)), uint32(len(payload)), checksum} {
		if err := binary.Write(&hdr, binary.LittleEndian, v); err != nil {
			return 0, 0, 0, err
		}
	}
	if _, err := f.Write(hdr.Bytes()); err != nil {
		return 0, 0, 0, err
	}
	if _, err := f.Write(payload); err != nil {
		return 0, 0, 0, err
	}
	return uint64(offset), uint32(len(payload)), checksum, nil
}

func (db *DB) compressChunkPayload(raw []byte) ([]byte, error) {
	if db.compressor != nil {
		return db.compressor.compress(raw, zstd.SpeedFastest)
	}
	return compressZstd(raw, zstd.SpeedFastest)
}

func writeChunkDataHeader(f *os.File, coord ChunkCoord) error {
	var buf bytes.Buffer
	buf.WriteString("CDAT")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, coord.X)
	_ = binary.Write(&buf, binary.LittleEndian, coord.Y)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(ChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_, err := f.Write(buf.Bytes())
	return err
}

func writeChunkTileDataHeader(f *os.File, tile chunkTileCoord) error {
	var buf bytes.Buffer
	buf.WriteString("TDAT")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, tile.X)
	_ = binary.Write(&buf, binary.LittleEndian, tile.Y)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(ChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_, err := f.Write(buf.Bytes())
	return err
}

func encodeChunkSnapshot(pixels [ChunkPixelCount]uint16) []byte {
	raw := make([]byte, chunkPayloadBytes)
	for i, color := range pixels {
		binary.LittleEndian.PutUint16(raw[i*2:i*2+2], color)
	}
	return raw
}

func decodeChunkSnapshot(raw []byte) ([ChunkPixelCount]uint16, error) {
	var pixels [ChunkPixelCount]uint16
	if len(raw) != chunkPayloadBytes {
		return pixels, fmt.Errorf("timeshadedb: chunk snapshot has %d bytes, expected %d", len(raw), chunkPayloadBytes)
	}
	for i := 0; i < ChunkPixelCount; i++ {
		pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return pixels, nil
}

func encodeChunkDeltaPayload(tick uint64, changes []chunkPixelChange) []byte {
	var buf bytes.Buffer
	buf.WriteString("CDEL")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(changes)))
	_ = binary.Write(&buf, binary.LittleEndian, tick)
	tmp := make([]byte, binary.MaxVarintLen64)
	for _, change := range changes {
		n := binary.PutUvarint(tmp, uint64(change.Pos))
		buf.Write(tmp[:n])
		_ = binary.Write(&buf, binary.LittleEndian, change.Color)
	}
	return buf.Bytes()
}

func decodeChunkDeltaPayload(raw []byte) (uint64, []chunkPixelChange, error) {
	if len(raw) < 18 || string(raw[:4]) != "CDEL" {
		return 0, nil, errors.New("timeshadedb: bad chunk delta payload")
	}
	version := binary.LittleEndian.Uint16(raw[4:6])
	if version != formatVersion {
		return 0, nil, fmt.Errorf("timeshadedb: unsupported chunk delta payload version %d", version)
	}
	count := binary.LittleEndian.Uint32(raw[6:10])
	tick := binary.LittleEndian.Uint64(raw[10:18])
	pos := 18
	changes := make([]chunkPixelChange, 0, count)
	for i := uint32(0); i < count; i++ {
		p, n := binary.Uvarint(raw[pos:])
		if n <= 0 {
			return 0, nil, errors.New("timeshadedb: bad chunk delta position varint")
		}
		pos += n
		if pos+2 > len(raw) {
			return 0, nil, errors.New("timeshadedb: truncated chunk delta color")
		}
		if p >= ChunkPixelCount {
			return 0, nil, fmt.Errorf("timeshadedb: chunk delta position out of bounds: %d", p)
		}
		changes = append(changes, chunkPixelChange{Pos: uint16(p), Color: binary.LittleEndian.Uint16(raw[pos : pos+2])})
		pos += 2
	}
	if pos != len(raw) {
		return 0, nil, errors.New("timeshadedb: trailing bytes in chunk delta payload")
	}
	return tick, changes, nil
}

func (db *DB) chunkDatastoreLocked(key DatastoreKey) *chunkDatastore {
	if db.chunkDatastores == nil {
		db.chunkDatastores = map[DatastoreKey]*chunkDatastore{}
	}
	ds := db.chunkDatastores[key]
	if ds == nil {
		ds = &chunkDatastore{
			key:    key,
			chunks: map[ChunkCoord]*factorioChunkState{},
			events: map[ChunkCoord][]chunkLogEntry{},
		}
		db.chunkDatastores[key] = ds
	}
	return ds
}

func (ds *chunkDatastore) chunkLocked(coord ChunkCoord) *factorioChunkState {
	chunk := ds.chunks[coord]
	if chunk == nil {
		chunk = &factorioChunkState{}
		ds.chunks[coord] = chunk
	}
	return chunk
}

func (db *DB) loadChunkDataFilesLocked() (bool, error) {
	root := filepath.Join(db.path, chunkDatastoresDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	loaded := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dsDir := filepath.Join(root, entry.Name())
		key, err := readChunkMetadata(dsDir)
		if err != nil {
			return false, err
		}
		ds := db.chunkDatastoreLocked(key)
		ds.dirsReady = true
		ds.metadataWritten = true
		var progress *chunkDatastoreProgress
		if savedProgress, err := readChunkProgress(dsDir); err == nil {
			progress = &savedProgress
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if loadedLegacy, err := db.loadLegacyChunkIndexFilesLocked(ds, dsDir); err != nil {
			return false, err
		} else if loadedLegacy {
			loaded = true
		}
		if loadedTiles, err := db.loadChunkTileIndexFilesLocked(ds, dsDir); err != nil {
			return false, err
		} else if loadedTiles {
			loaded = true
		}
		if progress != nil {
			ds.latestTick = progress.LatestTick
			ds.latestChunk = ChunkCoord{X: progress.LatestChunkX, Y: progress.LatestChunkY}
			ds.latestRowSeq = progress.LatestRowSeq
		}
	}
	return loaded, nil
}

func (db *DB) loadLegacyChunkIndexFilesLocked(ds *chunkDatastore, dsDir string) (bool, error) {
	chunksDir := filepath.Join(dsDir, "chunks")
	chunkFiles, err := os.ReadDir(chunksDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	loaded := false
	for _, file := range chunkFiles {
		if file.IsDir() || filepath.Ext(file.Name()) != ".cidx" {
			continue
		}
		idxPath := filepath.Join(chunksDir, file.Name())
		coord, idx, err := readChunkIndex(idxPath)
		if err != nil {
			return false, err
		}
		dataPath := chunkDataPath(db.path, ds.key, coord)
		if err := db.loadChunkFramesLocked(ds, coord, idx, false, dataPath, func(f *os.File) error {
			return readChunkDataHeader(f, coord)
		}); err != nil {
			return false, err
		}
		loaded = true
	}
	return loaded, nil
}

func (db *DB) loadChunkTileIndexFilesLocked(ds *chunkDatastore, dsDir string) (bool, error) {
	tilesDir := filepath.Join(dsDir, "tiles")
	tileFiles, err := os.ReadDir(tilesDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	loaded := false
	for _, file := range tileFiles {
		if file.IsDir() || filepath.Ext(file.Name()) != ".tidx" {
			continue
		}
		idxPath := filepath.Join(tilesDir, file.Name())
		tile, indexes, err := readChunkTileIndex(idxPath)
		if err != nil {
			return false, err
		}
		dataPath := chunkTileDataPath(db.path, ds.key, tile)
		for coord, idx := range indexes {
			if err := db.loadChunkFramesLocked(ds, coord, idx, true, dataPath, func(f *os.File) error {
				return readChunkTileDataHeader(f, tile)
			}); err != nil {
				return false, err
			}
			loaded = true
		}
	}
	return loaded, nil
}

func (db *DB) loadChunkFramesLocked(ds *chunkDatastore, coord ChunkCoord, idx chunkIndex, tileBacked bool, dataPath string, readHeader func(*os.File) error) error {
	f, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := readHeader(f); err != nil {
		return err
	}
	chunk := ds.chunkLocked(coord)
	chunk.index = idx
	chunk.tileBacked = tileBacked
	type indexedFrame struct {
		offset uint64
		seq    uint64
		kind   uint8
	}
	frames := make([]indexedFrame, 0, len(idx.snapshots)+len(idx.deltas))
	for _, snap := range idx.snapshots {
		frames = append(frames, indexedFrame{offset: snap.FrameOffset, seq: snap.EventSeq, kind: frameKindSnapshot})
	}
	for _, delta := range idx.deltas {
		frames = append(frames, indexedFrame{offset: delta.FrameOffset, seq: delta.LastEventSeq, kind: frameKindDelta})
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].offset < frames[j].offset })
	for _, frame := range frames {
		info, err := readChunkFrame(f, frame.offset)
		if err != nil {
			return err
		}
		raw, err := decompressZstd(info.payload, info.rawLen)
		if err != nil {
			return err
		}
		if crc32.ChecksumIEEE(raw) != info.checksum {
			return errors.New("timeshadedb: chunk frame checksum mismatch")
		}
		if info.kind != frame.kind {
			return fmt.Errorf("timeshadedb: chunk index kind mismatch: index %d frame %d", frame.kind, info.kind)
		}
		switch frame.kind {
		case frameKindSnapshot:
			pixels, err := decodeChunkSnapshot(raw)
			if err != nil {
				return err
			}
			chunk.pixels = pixels
			changes := make([]chunkPixelChange, 0, ChunkPixelCount)
			for i, color := range pixels {
				changes = append(changes, chunkPixelChange{Pos: uint16(i), Color: color})
			}
			db.applyLoadedChunkEntryLocked(ds, chunk, chunkLogEntry{
				Seq:     frame.seq,
				Tick:    info.tick,
				Key:     ds.key,
				Chunk:   coord,
				Changes: changes,
			})
		case frameKindDelta:
			tick, changes, err := decodeChunkDeltaPayload(raw)
			if err != nil {
				return err
			}
			if tick != info.tick {
				return fmt.Errorf("timeshadedb: chunk delta tick mismatch: payload %d frame %d", tick, info.tick)
			}
			for _, change := range changes {
				chunk.pixels[change.Pos] = change.Color
			}
			db.applyLoadedChunkEntryLocked(ds, chunk, chunkLogEntry{
				Seq:     frame.seq,
				Tick:    info.tick,
				Key:     ds.key,
				Chunk:   coord,
				Changes: changes,
			})
		default:
			return fmt.Errorf("timeshadedb: unsupported chunk frame kind %d", info.kind)
		}
	}
	chunk.observeSnapshotCountersFromIndex()
	return nil
}

func (db *DB) applyLoadedChunkEntryLocked(ds *chunkDatastore, chunk *factorioChunkState, entry chunkLogEntry) {
	chunk.seen = true
	chunk.latestTick = entry.Tick
	chunk.latestRowSeq = entry.Seq
	ds.events[entry.Chunk] = append(ds.events[entry.Chunk], entry)
	ds.latestTick = entry.Tick
	ds.latestChunk = entry.Chunk
	ds.latestRowSeq = entry.Seq
	if entry.Seq >= db.nextChunkSeq {
		db.nextChunkSeq = entry.Seq + 1
	}
}

func (chunk *factorioChunkState) observeSnapshotCountersFromIndex() {
	chunk.lastSnapshotBytes = 0
	chunk.deltaFramesSinceSnap = 0
	chunk.deltaEventsSinceSnap = 0
	chunk.deltaBytesSinceSnapshot = 0
	if len(chunk.index.snapshots) == 0 {
		return
	}
	lastSnapIdx := len(chunk.index.snapshots) - 1
	lastSnap := chunk.index.snapshots[lastSnapIdx]
	chunk.lastSnapshotBytes = lastSnap.CompressedLen
	end := uint32(len(chunk.index.deltas))
	if lastSnapIdx+1 < len(chunk.index.snapshots) {
		end = chunk.index.snapshots[lastSnapIdx+1].FirstDeltaFrameIdx
	}
	for di := lastSnap.FirstDeltaFrameIdx; di < end; di++ {
		delta := chunk.index.deltas[di]
		chunk.deltaFramesSinceSnap++
		chunk.deltaEventsSinceSnap += delta.EventCount
		chunk.deltaBytesSinceSnapshot += uint64(delta.CompressedLen)
	}
}

func readChunkDataHeader(f *os.File, coord ChunkCoord) error {
	hdr := make([]byte, 18)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return err
	}
	if string(hdr[:4]) != "CDAT" {
		return errors.New("timeshadedb: bad chunk data magic")
	}
	version := binary.LittleEndian.Uint16(hdr[4:6])
	x := int32(binary.LittleEndian.Uint32(hdr[6:10]))
	y := int32(binary.LittleEndian.Uint32(hdr[10:14]))
	size := binary.LittleEndian.Uint16(hdr[14:16])
	format := binary.LittleEndian.Uint16(hdr[16:18])
	if version != formatVersion || x != coord.X || y != coord.Y || size != ChunkSize || format != 1 {
		return errors.New("timeshadedb: chunk data header mismatch")
	}
	return nil
}

func readChunkTileDataHeader(f *os.File, tile chunkTileCoord) error {
	hdr := make([]byte, 18)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return err
	}
	if string(hdr[:4]) != "TDAT" {
		return errors.New("timeshadedb: bad chunk tile data magic")
	}
	version := binary.LittleEndian.Uint16(hdr[4:6])
	x := int32(binary.LittleEndian.Uint32(hdr[6:10]))
	y := int32(binary.LittleEndian.Uint32(hdr[10:14]))
	size := binary.LittleEndian.Uint16(hdr[14:16])
	format := binary.LittleEndian.Uint16(hdr[16:18])
	if version != formatVersion || x != tile.X || y != tile.Y || size != ChunkSize || format != 1 {
		return errors.New("timeshadedb: chunk tile data header mismatch")
	}
	return nil
}

func readChunkFrame(f *os.File, offset uint64) (*chunkFrameInfo, error) {
	hdr := make([]byte, chunkFrameHeaderLen)
	if _, err := f.ReadAt(hdr, int64(offset)); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != "CFRM" {
		return nil, errors.New("timeshadedb: bad chunk frame magic")
	}
	info := &chunkFrameInfo{
		kind:          hdr[4],
		tick:          binary.LittleEndian.Uint64(hdr[5:13]),
		maxTick:       binary.LittleEndian.Uint64(hdr[13:21]),
		eventCount:    binary.LittleEndian.Uint32(hdr[21:25]),
		rawLen:        binary.LittleEndian.Uint32(hdr[25:29]),
		compressedLen: binary.LittleEndian.Uint32(hdr[29:33]),
		checksum:      binary.LittleEndian.Uint32(hdr[33:37]),
	}
	info.payload = make([]byte, info.compressedLen)
	if _, err := f.ReadAt(info.payload, int64(offset)+int64(chunkFrameHeaderLen)); err != nil {
		return nil, err
	}
	return info, nil
}

func writeChunkIndex(root string, key DatastoreKey, coord ChunkCoord, idx chunkIndex) error {
	dataPath := chunkDataPath(root, key, coord)
	info, err := os.Stat(dataPath)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("CIDX")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, coord.X)
	_ = binary.Write(&buf, binary.LittleEndian, coord.Y)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(ChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(info.Size()))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(idx.snapshots)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(idx.deltas)))
	for _, snap := range idx.snapshots {
		fields := []any{snap.Tick, snap.FrameOffset, snap.FrameHeaderLen, snap.CompressedLen, snap.RawLen, snap.FirstDeltaFrameIdx, snap.EventSeq, snap.Checksum}
		for _, field := range fields {
			_ = binary.Write(&buf, binary.LittleEndian, field)
		}
	}
	for _, delta := range idx.deltas {
		fields := []any{delta.MinTick, delta.MaxTick, delta.FrameOffset, delta.FrameHeaderLen, delta.CompressedLen, delta.RawLen, delta.EventCount, delta.FirstEventSeq, delta.LastEventSeq, delta.Checksum}
		for _, field := range fields {
			_ = binary.Write(&buf, binary.LittleEndian, field)
		}
	}
	path := chunkIndexPath(root, key, coord)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readChunkIndex(path string) (ChunkCoord, chunkIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ChunkCoord{}, chunkIndex{}, err
	}
	r := bytes.NewReader(data)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return ChunkCoord{}, chunkIndex{}, err
	}
	if string(magic) != "CIDX" {
		return ChunkCoord{}, chunkIndex{}, errors.New("timeshadedb: bad chunk index magic")
	}
	var version uint16
	var coord ChunkCoord
	var size uint16
	var dataSize uint64
	var snapCount, deltaCount uint32
	fields := []any{&version, &coord.X, &coord.Y, &size, &dataSize, &snapCount, &deltaCount}
	for _, field := range fields {
		if err := binary.Read(r, binary.LittleEndian, field); err != nil {
			return ChunkCoord{}, chunkIndex{}, err
		}
	}
	if version != formatVersion || size != ChunkSize {
		return ChunkCoord{}, chunkIndex{}, errors.New("timeshadedb: chunk index header mismatch")
	}
	idx := chunkIndex{
		snapshots: make([]chunkSnapshotRecord, snapCount),
		deltas:    make([]chunkDeltaRecord, deltaCount),
	}
	for i := range idx.snapshots {
		fields := []any{
			&idx.snapshots[i].Tick,
			&idx.snapshots[i].FrameOffset,
			&idx.snapshots[i].FrameHeaderLen,
			&idx.snapshots[i].CompressedLen,
			&idx.snapshots[i].RawLen,
			&idx.snapshots[i].FirstDeltaFrameIdx,
			&idx.snapshots[i].EventSeq,
			&idx.snapshots[i].Checksum,
		}
		for _, field := range fields {
			if err := binary.Read(r, binary.LittleEndian, field); err != nil {
				return ChunkCoord{}, chunkIndex{}, err
			}
		}
	}
	for i := range idx.deltas {
		fields := []any{
			&idx.deltas[i].MinTick,
			&idx.deltas[i].MaxTick,
			&idx.deltas[i].FrameOffset,
			&idx.deltas[i].FrameHeaderLen,
			&idx.deltas[i].CompressedLen,
			&idx.deltas[i].RawLen,
			&idx.deltas[i].EventCount,
			&idx.deltas[i].FirstEventSeq,
			&idx.deltas[i].LastEventSeq,
			&idx.deltas[i].Checksum,
		}
		for _, field := range fields {
			if err := binary.Read(r, binary.LittleEndian, field); err != nil {
				return ChunkCoord{}, chunkIndex{}, err
			}
		}
	}
	if r.Len() != 0 {
		return ChunkCoord{}, chunkIndex{}, errors.New("timeshadedb: trailing bytes in chunk index")
	}
	return coord, idx, nil
}

func writeChunkTileIndex(root string, key DatastoreKey, tile chunkTileCoord, indexes map[ChunkCoord]chunkIndex) error {
	dataPath := chunkTileDataPath(root, key, tile)
	info, err := os.Stat(dataPath)
	if err != nil {
		return err
	}
	coords := make([]ChunkCoord, 0, len(indexes))
	for coord, idx := range indexes {
		if len(idx.snapshots) == 0 && len(idx.deltas) == 0 {
			continue
		}
		coords = append(coords, coord)
	}
	sort.Slice(coords, func(i, j int) bool {
		if coords[i].X != coords[j].X {
			return coords[i].X < coords[j].X
		}
		return coords[i].Y < coords[j].Y
	})
	var buf bytes.Buffer
	buf.WriteString("TIDX")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, tile.X)
	_ = binary.Write(&buf, binary.LittleEndian, tile.Y)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(ChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(info.Size()))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(coords)))
	for _, coord := range coords {
		idx := indexes[coord]
		_ = binary.Write(&buf, binary.LittleEndian, coord.X)
		_ = binary.Write(&buf, binary.LittleEndian, coord.Y)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(idx.snapshots)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(idx.deltas)))
		for _, snap := range idx.snapshots {
			fields := []any{snap.Tick, snap.FrameOffset, snap.FrameHeaderLen, snap.CompressedLen, snap.RawLen, snap.FirstDeltaFrameIdx, snap.EventSeq, snap.Checksum}
			for _, field := range fields {
				_ = binary.Write(&buf, binary.LittleEndian, field)
			}
		}
		for _, delta := range idx.deltas {
			fields := []any{delta.MinTick, delta.MaxTick, delta.FrameOffset, delta.FrameHeaderLen, delta.CompressedLen, delta.RawLen, delta.EventCount, delta.FirstEventSeq, delta.LastEventSeq, delta.Checksum}
			for _, field := range fields {
				_ = binary.Write(&buf, binary.LittleEndian, field)
			}
		}
	}
	path := chunkTileIndexPath(root, key, tile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readChunkTileIndex(path string) (chunkTileCoord, map[ChunkCoord]chunkIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return chunkTileCoord{}, nil, err
	}
	r := bytes.NewReader(data)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return chunkTileCoord{}, nil, err
	}
	if string(magic) != "TIDX" {
		return chunkTileCoord{}, nil, errors.New("timeshadedb: bad chunk tile index magic")
	}
	var version uint16
	var tile chunkTileCoord
	var size uint16
	var dataSize uint64
	var chunkCount uint32
	fields := []any{&version, &tile.X, &tile.Y, &size, &dataSize, &chunkCount}
	for _, field := range fields {
		if err := binary.Read(r, binary.LittleEndian, field); err != nil {
			return chunkTileCoord{}, nil, err
		}
	}
	if version != formatVersion || size != ChunkSize {
		return chunkTileCoord{}, nil, errors.New("timeshadedb: chunk tile index header mismatch")
	}
	indexes := make(map[ChunkCoord]chunkIndex, chunkCount)
	for i := uint32(0); i < chunkCount; i++ {
		var coord ChunkCoord
		var snapCount, deltaCount uint32
		fields := []any{&coord.X, &coord.Y, &snapCount, &deltaCount}
		for _, field := range fields {
			if err := binary.Read(r, binary.LittleEndian, field); err != nil {
				return chunkTileCoord{}, nil, err
			}
		}
		if chunkTileCoordForChunk(coord) != tile {
			return chunkTileCoord{}, nil, errors.New("timeshadedb: chunk tile index contains out-of-tile chunk")
		}
		idx := chunkIndex{
			snapshots: make([]chunkSnapshotRecord, snapCount),
			deltas:    make([]chunkDeltaRecord, deltaCount),
		}
		for i := range idx.snapshots {
			fields := []any{
				&idx.snapshots[i].Tick,
				&idx.snapshots[i].FrameOffset,
				&idx.snapshots[i].FrameHeaderLen,
				&idx.snapshots[i].CompressedLen,
				&idx.snapshots[i].RawLen,
				&idx.snapshots[i].FirstDeltaFrameIdx,
				&idx.snapshots[i].EventSeq,
				&idx.snapshots[i].Checksum,
			}
			for _, field := range fields {
				if err := binary.Read(r, binary.LittleEndian, field); err != nil {
					return chunkTileCoord{}, nil, err
				}
			}
		}
		for i := range idx.deltas {
			fields := []any{
				&idx.deltas[i].MinTick,
				&idx.deltas[i].MaxTick,
				&idx.deltas[i].FrameOffset,
				&idx.deltas[i].FrameHeaderLen,
				&idx.deltas[i].CompressedLen,
				&idx.deltas[i].RawLen,
				&idx.deltas[i].EventCount,
				&idx.deltas[i].FirstEventSeq,
				&idx.deltas[i].LastEventSeq,
				&idx.deltas[i].Checksum,
			}
			for _, field := range fields {
				if err := binary.Read(r, binary.LittleEndian, field); err != nil {
					return chunkTileCoord{}, nil, err
				}
			}
		}
		indexes[coord] = idx
	}
	if r.Len() != 0 {
		return chunkTileCoord{}, nil, errors.New("timeshadedb: trailing bytes in chunk tile index")
	}
	return tile, indexes, nil
}

func writeChunkMetadata(dsDir string, key DatastoreKey) error {
	data, err := marshalJSON(key)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dsDir, "metadata.json"), data, 0o644)
}

func writeChunkProgress(dsDir string, progress chunkDatastoreProgress) error {
	data, err := marshalJSON(progress)
	if err != nil {
		return err
	}
	path := filepath.Join(dsDir, "progress.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readChunkProgress(dsDir string) (chunkDatastoreProgress, error) {
	data, err := os.ReadFile(filepath.Join(dsDir, "progress.json"))
	if err != nil {
		return chunkDatastoreProgress{}, err
	}
	var progress chunkDatastoreProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return chunkDatastoreProgress{}, err
	}
	return progress, nil
}

func readChunkMetadata(dsDir string) (DatastoreKey, error) {
	data, err := os.ReadFile(filepath.Join(dsDir, "metadata.json"))
	if err != nil {
		return DatastoreKey{}, err
	}
	var key DatastoreKey
	if err := json.Unmarshal(data, &key); err != nil {
		return DatastoreKey{}, err
	}
	return key, validateDatastoreKey(key)
}

func chunkDatastoreID(key DatastoreKey) string {
	sum := sha256.Sum256([]byte(key.SavefileUUID + "\x00" + key.Surface + "\x00" + key.Force))
	return hex.EncodeToString(sum[:16])
}

func chunkDatastorePath(root string, key DatastoreKey) string {
	return filepath.Join(root, chunkDatastoresDir, chunkDatastoreID(key))
}

func chunkFileStem(coord ChunkCoord) string {
	return fmt.Sprintf("cx_%d_cy_%d", coord.X, coord.Y)
}

func chunkTileCoordForChunk(coord ChunkCoord) chunkTileCoord {
	chunksPerTile := TileSize / ChunkSize
	return chunkTileCoord{
		X: floorDiv32(coord.X, chunksPerTile),
		Y: floorDiv32(coord.Y, chunksPerTile),
	}
}

func chunkTileFileStem(tile chunkTileCoord) string {
	return fmt.Sprintf("tx_%d_ty_%d", tile.X, tile.Y)
}

func chunkIndexesForTile(ds *chunkDatastore, tile chunkTileCoord) map[ChunkCoord]chunkIndex {
	indexes := map[ChunkCoord]chunkIndex{}
	for coord, chunk := range ds.chunks {
		if chunkTileCoordForChunk(coord) == tile {
			indexes[coord] = chunk.index
		}
	}
	return indexes
}

func chunkDataPath(root string, key DatastoreKey, coord ChunkCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "chunks", chunkFileStem(coord)+".cdat")
}

func chunkIndexPath(root string, key DatastoreKey, coord ChunkCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "chunks", chunkFileStem(coord)+".cidx")
}

func chunkTileDataPath(root string, key DatastoreKey, tile chunkTileCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "tiles", chunkTileFileStem(tile)+".tdat")
}

func chunkTileIndexPath(root string, key DatastoreKey, tile chunkTileCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "tiles", chunkTileFileStem(tile)+".tidx")
}
