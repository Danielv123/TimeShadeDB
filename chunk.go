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

// ErrDatastoreNotFound is returned (wrapped) by chunk reads for a datastore key the
// database does not hold.
var ErrDatastoreNotFound = errors.New("timeshadedb: datastore not found")

// chunkDatastore holds one datastore's live chunk state. History is not kept in
// memory: reads replay frames from the tile files on disk.
type chunkDatastore struct {
	key    DatastoreKey
	chunks map[ChunkCoord]*factorioChunkState
	// tiles holds one entry per 512 px tile that has at least one chunk. Its lock
	// guards the tile's chunk states and files. Entries are only created by ingest
	// and load, never by reads, so hostile tile requests allocate nothing.
	tiles           map[chunkTileCoord]*chunkTileState
	latestTick      uint64
	latestChunk     ChunkCoord
	latestRowSeq    uint64
	maxTick         uint64
	dirsReady       bool
	metadataWritten bool
}

// chunkTileState is the lock and chunk list of one tile shard.
type chunkTileState struct {
	mu     sync.RWMutex
	chunks map[ChunkCoord]*factorioChunkState
}

type chunkDatastoreProgress struct {
	LatestTick   uint64 `json:"latest_tick"`
	LatestChunkX int32  `json:"latest_chunk_x"`
	LatestChunkY int32  `json:"latest_chunk_y"`
	LatestRowSeq uint64 `json:"latest_row_seq"`
}

type factorioChunkState struct {
	pixels       [ChunkPixelCount]uint16
	seen         bool
	latestTick   uint64
	latestRowSeq uint64
	index        chunkIndex
	tileBacked   bool
	// legacy is the index of the chunk's legacy per-chunk file (chunks/*.cdat), if
	// any. Its frames precede every frame in index.
	legacy                  *chunkIndex
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
	err      error
}

type chunkCopyDatastore struct {
	key          DatastoreKey
	latestTick   uint64
	latestChunk  ChunkCoord
	latestRowSeq uint64
	chunks       []chunkCopyChunk
}

type chunkCopyChunk struct {
	coord ChunkCoord
	plan  chunkFramePlan
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
	tileStates := make(map[chunkTileBatchKey]*chunkTileState, len(groupOrder))
	db.chunkMu.Lock()
	for _, key := range groupOrder {
		ds := db.chunkDatastoreLocked(key.key)
		tileStates[key] = ds.tileLocked(chunkTileCoord{X: key.tileX, Y: key.tileY})
	}
	db.chunkMu.Unlock()
	releaseTileLocks := acquireChunkTileLocks(groupOrder, tileStates)
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
	maxTicks := map[DatastoreKey]uint64{}
	for _, rows := range groups {
		for _, work := range rows {
			if work.row.Tick > maxTicks[work.row.Key] {
				maxTicks[work.row.Key] = work.row.Tick
			}
		}
	}
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
	}
	db.chunkMu.Lock()
	defer db.chunkMu.Unlock()
	for key, latest := range progress {
		ds := touchedDatastores[key]
		if latest.LatestRowSeq > ds.latestRowSeq {
			ds.latestTick = latest.LatestTick
			ds.latestChunk = ChunkCoord{X: latest.LatestChunkX, Y: latest.LatestChunkY}
			ds.latestRowSeq = latest.LatestRowSeq
		}
		if tick := maxTicks[key]; tick > ds.maxTick {
			ds.maxTick = tick
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
	reader := newChunkFrameReader()
	defer reader.close()
	for _, ds := range datastores {
		latestPixels := map[ChunkCoord][ChunkPixelCount]uint16{}
		for _, chunk := range ds.chunks {
			// Replay every stored frame of the chunk in row order, as the rows were
			// ingested. History is read from disk, one frame at a time.
			frames := chunk.plan.all()
			sort.SliceStable(frames, func(i, j int) bool {
				if frames[i].seq != frames[j].seq {
					return frames[i].seq < frames[j].seq
				}
				return frames[i].tick < frames[j].tick
			})
			var pixels [ChunkPixelCount]uint16
			for _, frame := range frames {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if _, err := reader.apply(frame, ^uint64(0), &pixels); err != nil {
					return nil, err
				}
				batch = append(batch, ParsedChunkRow{Key: ds.key, Tick: frame.tick, Chunk: chunk.coord, Pixels: pixels})
				if len(batch) >= chunkCopyBatchRows {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
			if len(frames) > 0 {
				latestPixels[chunk.coord] = pixels
				stats.Chunks++
			}
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

// snapshotChunkCopyDatastores captures, for every datastore, the frame plan of every
// chunk. Index slices are append-only, so the captured slice headers stay valid
// while ingest appends to them.
func (db *DB) snapshotChunkCopyDatastores(ctx context.Context) ([]chunkCopyDatastore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := db.ensureChunksLoaded(); err != nil {
		return nil, err
	}
	type chunkRef struct {
		coord ChunkCoord
		chunk *factorioChunkState
		tile  *chunkTileState
	}
	db.chunkMu.RLock()
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
	refs := make([][]chunkRef, 0, len(keys))
	for _, key := range keys {
		ds := db.chunkDatastores[key]
		out = append(out, chunkCopyDatastore{
			key:          key,
			latestTick:   ds.latestTick,
			latestChunk:  ds.latestChunk,
			latestRowSeq: ds.latestRowSeq,
		})
		dsRefs := make([]chunkRef, 0, len(ds.chunks))
		for coord, chunk := range ds.chunks {
			dsRefs = append(dsRefs, chunkRef{coord: coord, chunk: chunk, tile: ds.tiles[chunkTileCoordForChunk(coord)]})
		}
		sort.Slice(dsRefs, func(i, j int) bool {
			if dsRefs[i].coord.X != dsRefs[j].coord.X {
				return dsRefs[i].coord.X < dsRefs[j].coord.X
			}
			return dsRefs[i].coord.Y < dsRefs[j].coord.Y
		})
		refs = append(refs, dsRefs)
	}
	db.chunkMu.RUnlock()

	for i := range out {
		dsDir := chunkDatastorePath(db.path, out[i].key)
		chunks := make([]chunkCopyChunk, 0, len(refs[i]))
		for _, ref := range refs[i] {
			if ref.tile != nil {
				ref.tile.mu.RLock()
			}
			plan := newChunkFramePlan(dsDir, ref.coord, ref.chunk)
			if ref.tile != nil {
				ref.tile.mu.RUnlock()
			}
			chunks = append(chunks, chunkCopyChunk{coord: ref.coord, plan: plan})
		}
		out[i].chunks = chunks
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

// acquireChunkTileLocks write-locks the tiles in a fixed order, so concurrent ingests
// cannot deadlock. The tile states must exist (see chunkDatastore.tileLocked).
func acquireChunkTileLocks(keys []chunkTileBatchKey, tiles map[chunkTileBatchKey]*chunkTileState) func() {
	ordered := append([]chunkTileBatchKey(nil), keys...)
	sort.Slice(ordered, func(i, j int) bool {
		return compareChunkTileBatchKey(ordered[i], ordered[j]) < 0
	})
	locks := make([]*sync.RWMutex, 0, len(ordered))
	for _, key := range ordered {
		lock := &tiles[key].mu
		lock.Lock()
		locks = append(locks, lock)
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
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
		// Size the change list exactly: it is kept until the batch is written.
		changed := ChunkPixelCount
		if wasSeen {
			changed = 0
			for i, color := range pixels {
				if chunk.pixels[i] != color {
					changed++
				}
			}
		}
		var changes []chunkPixelChange
		if changed > 0 {
			changes = make([]chunkPixelChange, 0, changed)
		}
		for i, color := range pixels {
			if !wasSeen || chunk.pixels[i] != color {
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
	if err := db.ensureChunksLoaded(); err != nil {
		return nil, err
	}

	db.chunkMu.RLock()
	ds := db.chunkDatastores[key]
	var dsDir string
	if ds != nil {
		dsDir = chunkDatastorePath(db.path, key)
	}
	db.chunkMu.RUnlock()
	if ds == nil {
		return nil, fmt.Errorf("%w: %s/%s/%s", ErrDatastoreNotFound, key.SavefileUUID, key.Surface, key.Force)
	}

	groups := map[chunkTileCoord][]ChunkCoord{}
	for _, coord := range coords {
		tile := chunkTileCoordForChunk(coord)
		groups[tile] = append(groups[tile], coord)
	}
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
	results := map[ChunkCoord]*ChunkResult{}
	reader := newChunkFrameReader()
	defer reader.close()
	for _, tile := range tiles {
		if err := db.chunksAtInTile(ctx, ds, dsDir, tick, tile, groups[tile], reader, results); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// chunksAtInTile reads the chunks of one tile at a tick into results. Tiles that hold no
// chunk return immediately, without taking a lock or allocating. The tile lock is held
// only while chunk state is copied, never during disk IO.
func (db *DB) chunksAtInTile(ctx context.Context, ds *chunkDatastore, dsDir string, tick uint64, tile chunkTileCoord, coords []ChunkCoord, reader *chunkFrameReader, results map[ChunkCoord]*ChunkResult) error {
	db.chunkMu.RLock()
	ts := ds.tiles[tile]
	db.chunkMu.RUnlock()
	if ts == nil {
		return nil
	}
	key := ds.key
	var plans []chunkCopyChunk
	ts.mu.RLock()
	for _, coord := range coords {
		chunk := ts.chunks[coord]
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
		plans = append(plans, chunkCopyChunk{coord: coord, plan: newChunkFramePlan(dsDir, coord, chunk)})
	}
	ts.mu.RUnlock()

	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := p.plan.replayAt(reader, key, tick)
		if err != nil {
			return err
		}
		if result != nil {
			results[p.coord] = result
		}
	}
	return nil
}

// ensureChunksLoaded loads the chunk index on first use.
func (db *DB) ensureChunksLoaded() error {
	db.chunkMu.RLock()
	loaded := db.chunkLoaded
	db.chunkMu.RUnlock()
	if loaded {
		return nil
	}
	db.chunkMu.Lock()
	defer db.chunkMu.Unlock()
	return db.loadChunksLocked()
}

// ChunkDatastoreLatestTick returns the greatest tick ingested into the datastore, and
// whether the datastore exists. A chunk tile requested at a smaller tick will not
// change any more as long as rows arrive in tick order.
func (db *DB) ChunkDatastoreLatestTick(key DatastoreKey) (uint64, bool, error) {
	if err := db.ensureChunksLoaded(); err != nil {
		return 0, false, err
	}
	db.chunkMu.RLock()
	defer db.chunkMu.RUnlock()
	ds := db.chunkDatastores[key]
	if ds == nil {
		return 0, false, nil
	}
	tick := ds.maxTick
	if ds.latestTick > tick {
		tick = ds.latestTick
	}
	return tick, true, nil
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
	db.chunkMu.RLock()
	ts := ds.tiles[tile]
	db.chunkMu.RUnlock()
	return writeChunkTileIndex(db.path, ds.key, tile, chunkIndexesForTile(ts))
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

func (db *DB) chunkDatastoreLocked(key DatastoreKey) *chunkDatastore {
	if db.chunkDatastores == nil {
		db.chunkDatastores = map[DatastoreKey]*chunkDatastore{}
	}
	ds := db.chunkDatastores[key]
	if ds == nil {
		// Clone the key: it may be a substring of a whole ingested TSV line.
		key = DatastoreKey{
			SavefileUUID: strings.Clone(key.SavefileUUID),
			Surface:      strings.Clone(key.Surface),
			Force:        strings.Clone(key.Force),
		}
		ds = &chunkDatastore{
			key:    key,
			chunks: map[ChunkCoord]*factorioChunkState{},
			tiles:  map[chunkTileCoord]*chunkTileState{},
		}
		db.chunkDatastores[key] = ds
	}
	return ds
}

// tileLocked returns the tile state, creating it. Callers hold db.chunkMu for writing.
func (ds *chunkDatastore) tileLocked(tile chunkTileCoord) *chunkTileState {
	ts := ds.tiles[tile]
	if ts == nil {
		ts = &chunkTileState{chunks: map[ChunkCoord]*factorioChunkState{}}
		ds.tiles[tile] = ts
	}
	return ts
}

// chunkLocked returns the chunk state, creating it. Callers hold db.chunkMu for writing
// and, outside of load, the chunk's tile lock.
func (ds *chunkDatastore) chunkLocked(coord ChunkCoord) *factorioChunkState {
	chunk := ds.chunks[coord]
	if chunk == nil {
		chunk = &factorioChunkState{}
		ds.chunks[coord] = chunk
		ds.tileLocked(chunkTileCoordForChunk(coord)).chunks[coord] = chunk
	}
	return chunk
}

// loadChunkDataFilesLocked reads every datastore's tile and legacy chunk indexes and
// rebuilds each chunk's live pixels from its newest snapshot and the deltas after it.
// No other history is decoded or kept in memory.
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
	reader := newChunkFrameReader()
	defer reader.close()
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
		if err := db.restoreChunkStatesLocked(ds, dsDir, reader); err != nil {
			return false, err
		}
		reader.close()
		reader = newChunkFrameReader()
		if progress != nil {
			ds.latestTick = progress.LatestTick
			ds.latestChunk = ChunkCoord{X: progress.LatestChunkX, Y: progress.LatestChunkY}
			ds.latestRowSeq = progress.LatestRowSeq
		}
		if ds.latestTick > ds.maxTick {
			ds.maxTick = ds.latestTick
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
		coord, idx, err := readChunkIndex(filepath.Join(chunksDir, file.Name()))
		if err != nil {
			return false, err
		}
		if len(idx.snapshots) == 0 && len(idx.deltas) == 0 {
			continue
		}
		chunk := ds.chunkLocked(coord)
		chunk.legacy = &idx
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
		_, indexes, err := readChunkTileIndex(filepath.Join(tilesDir, file.Name()))
		if err != nil {
			return false, err
		}
		for coord, idx := range indexes {
			if len(idx.snapshots) == 0 && len(idx.deltas) == 0 {
				continue
			}
			chunk := ds.chunkLocked(coord)
			chunk.index = idx
			chunk.tileBacked = true
			loaded = true
		}
	}
	return loaded, nil
}

// restoreChunkStatesLocked rebuilds the live state of every chunk of a datastore from
// disk: pixels from the newest snapshot and the deltas after it, the newest frame's
// tick and sequence number, and the snapshot heuristic counters.
func (db *DB) restoreChunkStatesLocked(ds *chunkDatastore, dsDir string, reader *chunkFrameReader) error {
	coords := make([]ChunkCoord, 0, len(ds.chunks))
	for coord := range ds.chunks {
		coords = append(coords, coord)
	}
	// Tile order keeps reads within one file together.
	sort.Slice(coords, func(i, j int) bool {
		ti, tj := chunkTileCoordForChunk(coords[i]), chunkTileCoordForChunk(coords[j])
		if ti != tj {
			if ti.X != tj.X {
				return ti.X < tj.X
			}
			return ti.Y < tj.Y
		}
		if coords[i].X != coords[j].X {
			return coords[i].X < coords[j].X
		}
		return coords[i].Y < coords[j].Y
	})
	var newest chunkFrameRef
	var newestCoord ChunkCoord
	found := false
	for _, coord := range coords {
		chunk := ds.chunks[coord]
		plan := newChunkFramePlan(dsDir, coord, chunk)
		last, ok := plan.last()
		if !ok {
			continue
		}
		frames, _ := plan.framesAt(^uint64(0))
		var pixels [ChunkPixelCount]uint16
		for _, frame := range frames {
			if _, err := reader.apply(frame, ^uint64(0), &pixels); err != nil {
				return err
			}
		}
		chunk.pixels = pixels
		chunk.seen = true
		chunk.latestTick = last.tick
		chunk.latestRowSeq = last.seq
		if last.tick > ds.maxTick {
			ds.maxTick = last.tick
		}
		if seq := plan.maxSeq(); seq >= db.nextChunkSeq {
			db.nextChunkSeq = seq + 1
		}
		if !found || last.seq > newest.seq {
			newest, newestCoord, found = last, coord, true
		}
		chunk.observeSnapshotCountersFromIndex()
	}
	if found {
		ds.latestTick = newest.tick
		ds.latestChunk = newestCoord
		ds.latestRowSeq = newest.seq
	}
	return nil
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

// chunkIndexesForTile returns the tile index of every chunk in the tile. Callers hold
// the tile lock.
func chunkIndexesForTile(ts *chunkTileState) map[ChunkCoord]chunkIndex {
	indexes := make(map[ChunkCoord]chunkIndex, len(ts.chunks))
	for coord, chunk := range ts.chunks {
		indexes[coord] = chunk.index
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

// chunkFrameRef locates one stored chunk frame.
type chunkFrameRef struct {
	path    string
	legacy  bool // the frame lives in a legacy per-chunk .cdat file
	coord   ChunkCoord
	offset  uint64
	compLen uint32
	rawLen  uint32
	kind    uint8
	tick    uint64
	seq     uint64
}

// chunkFramePlan is the stored history of one chunk: the frames of its legacy
// per-chunk file (if any), followed by the frames of its tile shard. It holds slice
// headers of the chunk's append-only indexes, captured under the tile lock.
type chunkFramePlan struct {
	coord      ChunkCoord
	legacyPath string
	legacy     chunkIndex
	tilePath   string
	index      chunkIndex
}

func newChunkFramePlan(dsDir string, coord ChunkCoord, chunk *factorioChunkState) chunkFramePlan {
	plan := chunkFramePlan{coord: coord, index: chunk.index}
	if chunk.tileBacked || len(chunk.index.snapshots) > 0 || len(chunk.index.deltas) > 0 {
		plan.tilePath = filepath.Join(dsDir, "tiles", chunkTileFileStem(chunkTileCoordForChunk(coord))+".tdat")
	}
	if chunk.legacy != nil {
		plan.legacy = *chunk.legacy
		plan.legacyPath = filepath.Join(dsDir, "chunks", chunkFileStem(coord)+".cdat")
	}
	return plan
}

func (p *chunkFramePlan) snapshotRef(legacy bool, snap chunkSnapshotRecord) chunkFrameRef {
	path := p.tilePath
	if legacy {
		path = p.legacyPath
	}
	return chunkFrameRef{
		path:    path,
		legacy:  legacy,
		coord:   p.coord,
		offset:  snap.FrameOffset,
		compLen: snap.CompressedLen,
		rawLen:  snap.RawLen,
		kind:    frameKindSnapshot,
		tick:    snap.Tick,
		seq:     snap.EventSeq,
	}
}

func (p *chunkFramePlan) deltaRef(legacy bool, delta chunkDeltaRecord) chunkFrameRef {
	path := p.tilePath
	if legacy {
		path = p.legacyPath
	}
	return chunkFrameRef{
		path:    path,
		legacy:  legacy,
		coord:   p.coord,
		offset:  delta.FrameOffset,
		compLen: delta.CompressedLen,
		rawLen:  delta.RawLen,
		kind:    frameKindDelta,
		tick:    delta.MinTick,
		seq:     delta.LastEventSeq,
	}
}

// segment appends the frames of one index in write order: deltas recorded before a
// snapshot precede it, deltas after the last snapshot follow it.
func (p *chunkFramePlan) segment(legacy bool, idx chunkIndex, out []chunkFrameRef) []chunkFrameRef {
	di := 0
	for _, snap := range idx.snapshots {
		for ; di < int(snap.FirstDeltaFrameIdx) && di < len(idx.deltas); di++ {
			out = append(out, p.deltaRef(legacy, idx.deltas[di]))
		}
		out = append(out, p.snapshotRef(legacy, snap))
	}
	for ; di < len(idx.deltas); di++ {
		out = append(out, p.deltaRef(legacy, idx.deltas[di]))
	}
	return out
}

// all returns every frame of the chunk in write order.
func (p *chunkFramePlan) all() []chunkFrameRef {
	out := make([]chunkFrameRef, 0, len(p.legacy.snapshots)+len(p.legacy.deltas)+len(p.index.snapshots)+len(p.index.deltas))
	out = p.segment(true, p.legacy, out)
	return p.segment(false, p.index, out)
}

type chunkPlanSegment struct {
	legacy bool
	idx    chunkIndex
}

// segmentsNewestFirst returns the tile segment, then the legacy one.
func (p *chunkFramePlan) segmentsNewestFirst() [2]chunkPlanSegment {
	return [2]chunkPlanSegment{{false, p.index}, {true, p.legacy}}
}

// framesAt returns the frames to replay for the chunk's state at tick. When fromZero
// is true no snapshot at or before tick exists, and the frames replay from black up to
// the first frame after tick (what the former in-memory event log did). Otherwise the
// first frame is the newest snapshot at or before tick, followed by its deltas up to
// tick.
func (p *chunkFramePlan) framesAt(tick uint64) (frames []chunkFrameRef, fromZero bool) {
	for _, seg := range p.segmentsNewestFirst() {
		snapIdx := latestChunkSnapshotAtOrBefore(seg.idx.snapshots, tick)
		if snapIdx < 0 {
			continue
		}
		snap := seg.idx.snapshots[snapIdx]
		end := uint32(len(seg.idx.deltas))
		if snapIdx+1 < len(seg.idx.snapshots) {
			end = seg.idx.snapshots[snapIdx+1].FirstDeltaFrameIdx
		}
		frames = append(frames, p.snapshotRef(seg.legacy, snap))
		for di := snap.FirstDeltaFrameIdx; di < end; di++ {
			delta := seg.idx.deltas[di]
			if delta.MinTick > tick {
				break
			}
			frames = append(frames, p.deltaRef(seg.legacy, delta))
		}
		return frames, false
	}
	for _, frame := range p.all() {
		if frame.tick > tick {
			break
		}
		frames = append(frames, frame)
	}
	return frames, true
}

// last returns the chunk's newest frame in write order.
func (p *chunkFramePlan) last() (chunkFrameRef, bool) {
	for _, seg := range p.segmentsNewestFirst() {
		nSnap, nDelta := len(seg.idx.snapshots), len(seg.idx.deltas)
		if nSnap > 0 && int(seg.idx.snapshots[nSnap-1].FirstDeltaFrameIdx) >= nDelta {
			return p.snapshotRef(seg.legacy, seg.idx.snapshots[nSnap-1]), true
		}
		if nDelta > 0 {
			return p.deltaRef(seg.legacy, seg.idx.deltas[nDelta-1]), true
		}
	}
	return chunkFrameRef{}, false
}

// maxSeq returns the greatest row sequence number of any frame.
func (p *chunkFramePlan) maxSeq() uint64 {
	var out uint64
	for _, seg := range p.segmentsNewestFirst() {
		for _, snap := range seg.idx.snapshots {
			out = max(out, snap.EventSeq)
		}
		for _, delta := range seg.idx.deltas {
			out = max(out, delta.LastEventSeq)
		}
	}
	return out
}

// replayAt rebuilds the chunk's state at tick from disk. It returns nil when the
// chunk has no data at or before tick.
func (p *chunkFramePlan) replayAt(reader *chunkFrameReader, key DatastoreKey, tick uint64) (*ChunkResult, error) {
	frames, fromZero := p.framesAt(tick)
	if len(frames) == 0 {
		return nil, nil
	}
	var pixels [ChunkPixelCount]uint16
	replayed := 0
	snapshotTick := frames[0].tick
	for _, frame := range frames {
		n, err := reader.apply(frame, tick, &pixels)
		if err != nil {
			return nil, err
		}
		replayed += n
		if fromZero {
			snapshotTick = frame.tick
		}
	}
	out := make([]uint16, ChunkPixelCount)
	copy(out, pixels[:])
	return &ChunkResult{
		Key:          key,
		Chunk:        p.coord,
		Tick:         tick,
		Width:        ChunkSize,
		Height:       ChunkSize,
		Pixels:       out,
		SnapshotTick: snapshotTick,
		Replayed:     replayed,
	}, nil
}

func latestChunkSnapshotAtOrBefore(snaps []chunkSnapshotRecord, tick uint64) int {
	i := sort.Search(len(snaps), func(i int) bool {
		return snaps[i].Tick > tick
	})
	return i - 1
}

// chunkFrameReader reads and applies frames. It keeps the files it opened (with their
// headers checked) and reuses its buffers. It is not safe for concurrent use.
type chunkFrameReader struct {
	files map[string]*os.File
	buf   []byte
	raw   []byte
}

type chunkFrameBufs struct {
	buf []byte
	raw []byte
}

var chunkFrameBufPool = sync.Pool{New: func() any { return new(chunkFrameBufs) }}

func newChunkFrameReader() *chunkFrameReader {
	bufs := chunkFrameBufPool.Get().(*chunkFrameBufs)
	return &chunkFrameReader{buf: bufs.buf, raw: bufs.raw}
}

func (r *chunkFrameReader) close() {
	for _, f := range r.files {
		_ = f.Close()
	}
	r.files = nil
	if cap(r.buf) <= 1<<20 && cap(r.raw) <= 1<<20 {
		chunkFrameBufPool.Put(&chunkFrameBufs{buf: r.buf[:0], raw: r.raw[:0]})
	}
	r.buf, r.raw = nil, nil
}

func (r *chunkFrameReader) file(ref chunkFrameRef) (*os.File, error) {
	if f := r.files[ref.path]; f != nil {
		return f, nil
	}
	if len(r.files) >= 16 {
		// Bound the open handles when a read spans many tiles.
		for _, f := range r.files {
			_ = f.Close()
		}
		clear(r.files)
	}
	f, err := os.Open(ref.path)
	if err != nil {
		return nil, err
	}
	if ref.legacy {
		err = readChunkDataHeader(f, ref.coord)
	} else {
		err = readChunkTileDataHeader(f, chunkTileCoordForChunk(ref.coord))
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if r.files == nil {
		r.files = map[string]*os.File{}
	}
	r.files[ref.path] = f
	return f, nil
}

// apply reads one frame with a single positioned read (header and payload together),
// checks it and applies it to pixels. A delta newer than maxTick is checked but not
// applied. It returns the number of pixel changes a delta applied (0 for a snapshot).
func (r *chunkFrameReader) apply(ref chunkFrameRef, maxTick uint64, pixels *[ChunkPixelCount]uint16) (int, error) {
	f, err := r.file(ref)
	if err != nil {
		return 0, err
	}
	hdrLen := int(chunkFrameHeaderLen)
	buf, err := r.readAt(f, int64(ref.offset), hdrLen+int(ref.compLen))
	if err != nil {
		return 0, err
	}
	if string(buf[:4]) != "CFRM" {
		return 0, errors.New("timeshadedb: bad chunk frame magic")
	}
	kind := buf[4]
	tick := binary.LittleEndian.Uint64(buf[5:13])
	rawLen := binary.LittleEndian.Uint32(buf[25:29])
	compLen := binary.LittleEndian.Uint32(buf[29:33])
	checksum := binary.LittleEndian.Uint32(buf[33:37])
	if compLen != ref.compLen {
		// The index disagrees with the frame header: trust the header, as the
		// two-read frame reader did.
		if buf, err = r.readAt(f, int64(ref.offset), hdrLen+int(compLen)); err != nil {
			return 0, err
		}
	}
	if kind != ref.kind {
		return 0, fmt.Errorf("timeshadedb: chunk index kind mismatch: index %d frame %d", ref.kind, kind)
	}
	raw, err := zstdDecoder().DecodeAll(buf[hdrLen:], r.raw[:0])
	if err != nil {
		return 0, err
	}
	r.raw = raw
	if len(raw) != int(rawLen) {
		return 0, fmt.Errorf("timeshadedb: decoded payload length %d, expected %d", len(raw), rawLen)
	}
	if crc32.ChecksumIEEE(raw) != checksum {
		return 0, errors.New("timeshadedb: chunk frame checksum mismatch")
	}
	switch kind {
	case frameKindSnapshot:
		if len(raw) != chunkPayloadBytes {
			return 0, fmt.Errorf("timeshadedb: chunk snapshot has %d bytes, expected %d", len(raw), chunkPayloadBytes)
		}
		for i := range pixels {
			pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
		}
		return 0, nil
	case frameKindDelta:
		return applyChunkDeltaPayload(raw, tick, maxTick, pixels)
	default:
		return 0, fmt.Errorf("timeshadedb: unsupported chunk frame kind %d", kind)
	}
}

func (r *chunkFrameReader) readAt(f *os.File, offset int64, n int) ([]byte, error) {
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	buf := r.buf[:n]
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// applyChunkDeltaPayload checks a delta payload (magic, version, tick, positions, no
// trailing bytes) and, when frameTick <= maxTick, applies it to pixels without
// allocating.
func applyChunkDeltaPayload(raw []byte, frameTick, maxTick uint64, pixels *[ChunkPixelCount]uint16) (int, error) {
	if len(raw) < 18 || string(raw[:4]) != "CDEL" {
		return 0, errors.New("timeshadedb: bad chunk delta payload")
	}
	version := binary.LittleEndian.Uint16(raw[4:6])
	if version != formatVersion {
		return 0, fmt.Errorf("timeshadedb: unsupported chunk delta payload version %d", version)
	}
	count := binary.LittleEndian.Uint32(raw[6:10])
	tick := binary.LittleEndian.Uint64(raw[10:18])
	if tick != frameTick {
		return 0, fmt.Errorf("timeshadedb: chunk delta tick mismatch: payload %d frame %d", tick, frameTick)
	}
	apply := frameTick <= maxTick
	pos := 18
	for i := uint32(0); i < count; i++ {
		p, n := binary.Uvarint(raw[pos:])
		if n <= 0 {
			return 0, errors.New("timeshadedb: bad chunk delta position varint")
		}
		pos += n
		if pos+2 > len(raw) {
			return 0, errors.New("timeshadedb: truncated chunk delta color")
		}
		if p >= ChunkPixelCount {
			return 0, fmt.Errorf("timeshadedb: chunk delta position out of bounds: %d", p)
		}
		if apply {
			pixels[p] = binary.LittleEndian.Uint16(raw[pos : pos+2])
		}
		pos += 2
	}
	if pos != len(raw) {
		return 0, errors.New("timeshadedb: trailing bytes in chunk delta payload")
	}
	if !apply {
		return 0, nil
	}
	return int(count), nil
}
