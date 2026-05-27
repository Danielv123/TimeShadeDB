package timeshadedb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChunkTSVRowSupportsCombinedAndSplitCoords(t *testing.T) {
	payload := repeatedRGB565Hex(0x2462)
	combined, err := ParseChunkTSVRow("save-1", "12\tnauvis\t-7,-6\tplayer\t"+payload)
	if err != nil {
		t.Fatal(err)
	}
	if combined.Tick != 12 || combined.Chunk.X != -7 || combined.Chunk.Y != -6 {
		t.Fatalf("combined parse = tick %d chunk %d,%d", combined.Tick, combined.Chunk.X, combined.Chunk.Y)
	}
	if combined.Key.Surface != "nauvis" || combined.Key.Force != "player" {
		t.Fatalf("combined key = %+v", combined.Key)
	}
	if combined.Pixels[0] != 0x2462 || combined.Pixels[ChunkPixelCount-1] != 0x2462 {
		t.Fatalf("combined pixels not decoded as little-endian hex RGB565")
	}

	split, err := ParseChunkTSVRow("save-1", "13\tnauvis\t-7\t-6\tplayer\t"+payload)
	if err != nil {
		t.Fatal(err)
	}
	if split.Tick != 13 || split.Chunk.X != -7 || split.Chunk.Y != -6 {
		t.Fatalf("split parse = tick %d chunk %d,%d", split.Tick, split.Chunk.X, split.Chunk.Y)
	}
}

func TestParseChunkTSVRowEmptyPayloadIsBlack(t *testing.T) {
	row, err := ParseChunkTSVRow("save-1", "14\tnauvis\t-7,-6\tplayer\t")
	if err != nil {
		t.Fatal(err)
	}
	for i, pixel := range row.Pixels {
		if pixel != 0 {
			t.Fatalf("pixel %d = %#04x, want black", i, pixel)
		}
	}
}

func TestIngestChunkStoresChangesMetadataHistoryAndReloads(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: dbPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	assertPathMissing(t, filepath.Join(dbPath, "tiles"))
	assertPathMissing(t, filepath.Join(dbPath, "wal"))
	assertPathMissing(t, filepath.Join(dbPath, "factorio_chunks"))
	assertPathMissing(t, filepath.Join(dbPath, "palette.bin"))
	assertPathMissing(t, filepath.Join(dbPath, "stats.json"))
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	first := make([]uint16, ChunkPixelCount)
	for i := range first {
		first[i] = 0x2462
	}
	res, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 10, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: first})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChangedPixels != ChunkPixelCount || res.UnchangedPixels != 0 {
		t.Fatalf("first ingest changed=%d unchanged=%d", res.ChangedPixels, res.UnchangedPixels)
	}
	duplicate, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 11, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: first})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ChangedPixels != 0 || duplicate.UnchangedPixels != ChunkPixelCount {
		t.Fatalf("duplicate ingest changed=%d unchanged=%d", duplicate.ChangedPixels, duplicate.UnchangedPixels)
	}
	second := append([]uint16(nil), first...)
	second[3] = 0xcdac
	third := append([]uint16(nil), second...)
	third[4] = 0x844a
	batchRows := []ParsedChunkRow{
		{Key: key, Tick: 12, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: sliceToChunkPixels(second)},
		{Key: key, Tick: 13, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: sliceToChunkPixels(third)},
	}
	changed, err := db.IngestChunkRows(ctx, batchRows)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ChangedPixels != 2 || changed.UnchangedPixels != 2*ChunkPixelCount-2 {
		t.Fatalf("second ingest changed=%d unchanged=%d", changed.ChangedPixels, changed.UnchangedPixels)
	}
	historical, err := db.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 11, Chunk: ChunkCoord{X: -7, Y: -6}})
	if err != nil {
		t.Fatal(err)
	}
	if historical.Pixels[3] != 0x2462 {
		t.Fatalf("historical pixel = %#04x, want 0x2462", historical.Pixels[3])
	}
	latest, err := db.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 12, Chunk: ChunkCoord{X: -7, Y: -6}})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Pixels[3] != 0xcdac {
		t.Fatalf("latest pixel = %#04x, want 0xcdac", latest.Pixels[3])
	}
	if latest.Pixels[4] != 0x2462 {
		t.Fatalf("tick 12 pixel 4 = %#04x, want 0x2462", latest.Pixels[4])
	}
	tick13, err := db.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 13, Chunk: ChunkCoord{X: -7, Y: -6}})
	if err != nil {
		t.Fatal(err)
	}
	if tick13.Pixels[4] != 0x844a {
		t.Fatalf("tick 13 pixel 4 = %#04x, want 0x844a", tick13.Pixels[4])
	}
	meta, err := db.IngestMetadata("save-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Datastores) != 1 || meta.Datastores[0].LatestTick != 13 || meta.Datastores[0].LatestChunkX != -7 {
		t.Fatalf("metadata = %+v", meta)
	}
	if _, err := os.Stat(filepath.Join(dbPath, "datastores")); err != nil {
		t.Fatalf("datastores directory missing: %v", err)
	}
	noChange, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 14, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: third})
	if err != nil {
		t.Fatal(err)
	}
	if noChange.ChangedPixels != 0 || noChange.UnchangedPixels != ChunkPixelCount {
		t.Fatalf("no-change ingest changed=%d unchanged=%d", noChange.ChangedPixels, noChange.UnchangedPixels)
	}
	sameTile := make([]uint16, ChunkPixelCount)
	for i := range sameTile {
		sameTile[i] = 0xf800
	}
	sameTileCoord := ChunkCoord{X: -8, Y: -6}
	sameTileResult, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 15, Chunk: sameTileCoord, Pixels: sameTile})
	if err != nil {
		t.Fatal(err)
	}
	if sameTileResult.ChangedPixels != ChunkPixelCount {
		t.Fatalf("same-tile ingest changed=%d, want %d", sameTileResult.ChangedPixels, ChunkPixelCount)
	}
	tile := chunkTileCoordForChunk(ChunkCoord{X: -7, Y: -6})
	if chunkTileCoordForChunk(sameTileCoord) != tile {
		t.Fatal("test chunks should share one 512x512 tile")
	}
	dataPath := chunkTileDataPath(dbPath, key, tile)
	indexPath := chunkTileIndexPath(dbPath, key, tile)
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("chunk tile data file missing: %v", err)
	}
	if _, err := os.Stat(chunkDataPath(dbPath, key, ChunkCoord{X: -7, Y: -6})); !os.IsNotExist(err) {
		t.Fatalf("legacy chunk data file exists or stat failed unexpectedly: %v", err)
	}
	_, indexes, err := readChunkTileIndex(indexPath)
	if err != nil {
		t.Fatalf("chunk tile index unreadable: %v", err)
	}
	idx := indexes[ChunkCoord{X: -7, Y: -6}]
	if len(idx.snapshots) != 1 || len(idx.deltas) != 2 {
		t.Fatalf("chunk index snapshots=%d deltas=%d, want 1 snapshot and 2 deltas", len(idx.snapshots), len(idx.deltas))
	}
	if _, ok := indexes[sameTileCoord]; !ok {
		t.Fatalf("same-tile chunk missing from tile index")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	afterReload, err := reopened.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 13, Chunk: ChunkCoord{X: -7, Y: -6}})
	if err != nil {
		t.Fatal(err)
	}
	if afterReload.Pixels[3] != 0xcdac {
		t.Fatalf("reloaded pixel = %#04x, want 0xcdac", afterReload.Pixels[3])
	}
	if afterReload.Pixels[4] != 0x844a {
		t.Fatalf("reloaded pixel 4 = %#04x, want 0x844a", afterReload.Pixels[4])
	}
	reloadedMeta, err := reopened.IngestMetadata("save-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadedMeta.Datastores) != 1 || reloadedMeta.Datastores[0].LatestTick != 15 || reloadedMeta.Datastores[0].LatestRowSeq != 6 {
		t.Fatalf("reloaded metadata = %+v", reloadedMeta)
	}
}

func TestLoadLegacyPerChunkFiles(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "db.tshd")
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	coord := ChunkCoord{X: -7, Y: -6}
	if err := os.MkdirAll(filepath.Dir(chunkDataPath(dbPath, key, coord)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeChunkManifest(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := writeChunkMetadata(chunkDatastorePath(dbPath, key), key); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(chunkDataPath(dbPath, key, coord), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeChunkDataHeader(f, coord); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	var pixels [ChunkPixelCount]uint16
	for i := range pixels {
		pixels[i] = 0x2462
	}
	writer := &DB{compressor: newCompressionPool()}
	raw := encodeChunkSnapshot(pixels)
	offset, compLen, checksum, err := writer.writeChunkFrame(f, frameKindSnapshot, 10, uint32(ChunkPixelCount), raw)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	idx := chunkIndex{snapshots: []chunkSnapshotRecord{{
		Tick:               10,
		FrameOffset:        offset,
		FrameHeaderLen:     chunkFrameHeaderLen,
		CompressedLen:      compLen,
		RawLen:             uint32(len(raw)),
		FirstDeltaFrameIdx: 0,
		EventSeq:           1,
		Checksum:           checksum,
	}}}
	if err := writeChunkIndex(dbPath, key, coord, idx); err != nil {
		t.Fatal(err)
	}

	db, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 10, Chunk: coord})
	if err != nil {
		t.Fatal(err)
	}
	if got.Pixels[0] != 0x2462 || got.Pixels[ChunkPixelCount-1] != 0x2462 {
		t.Fatalf("legacy chunk pixels were not loaded")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(filepath.Dir(dbPath), "copy.tshd")
	srcCopy, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	dstCopy, err := Open(OpenOptions{Path: copyPath, Format: FormatChunks})
	if err != nil {
		_ = srcCopy.Close()
		t.Fatal(err)
	}
	copyStats, err := srcCopy.CopyChunksTo(ctx, dstCopy)
	if closeErr := dstCopy.Close(); err == nil {
		err = closeErr
	}
	if closeErr := srcCopy.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if copyStats.Datastores != 1 || copyStats.Chunks != 1 || copyStats.RowsCopied == 0 {
		t.Fatalf("copy stats = %+v", copyStats)
	}
	if _, err := os.Stat(chunkTileDataPath(copyPath, key, chunkTileCoordForChunk(coord))); err != nil {
		t.Fatalf("copied tile shard missing: %v", err)
	}
	if _, err := os.Stat(chunkDataPath(copyPath, key, coord)); !os.IsNotExist(err) {
		t.Fatalf("copied DB should not contain legacy chunk data file: %v", err)
	}
	copiedDB, err := Open(OpenOptions{Path: copyPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	copiedChunk, err := copiedDB.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 10, Chunk: coord})
	if closeErr := copiedDB.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if copiedChunk.Pixels[0] != 0x2462 || copiedChunk.Pixels[ChunkPixelCount-1] != 0x2462 {
		t.Fatalf("copied chunk pixels were not loaded")
	}

	rw, err := Open(OpenOptions{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	updated := make([]uint16, ChunkPixelCount)
	for i := range updated {
		updated[i] = 0x2462
	}
	updated[0] = 0xf800
	if _, err := rw.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 11, Chunk: coord, Pixels: updated}); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	tile := chunkTileCoordForChunk(coord)
	_, tileIndexes, err := readChunkTileIndex(chunkTileIndexPath(dbPath, key, tile))
	if err != nil {
		t.Fatalf("tile index after legacy migration unreadable: %v", err)
	}
	migrated := tileIndexes[coord]
	if len(migrated.snapshots) != 1 || len(migrated.deltas) != 0 {
		t.Fatalf("migrated tile index snapshots=%d deltas=%d, want one fresh tile snapshot", len(migrated.snapshots), len(migrated.deltas))
	}

	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	oldTick, err := reopened.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 10, Chunk: coord})
	if err != nil {
		t.Fatal(err)
	}
	if oldTick.Pixels[0] != 0x2462 {
		t.Fatalf("legacy history after migration = %#04x, want 0x2462", oldTick.Pixels[0])
	}
	newTick, err := reopened.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 11, Chunk: coord})
	if err != nil {
		t.Fatal(err)
	}
	if newTick.Pixels[0] != 0xf800 || newTick.Pixels[1] != 0x2462 {
		t.Fatalf("migrated tile snapshot pixels = %#04x %#04x, want 0xf800 0x2462", newTick.Pixels[0], newTick.Pixels[1])
	}
}

func TestChunksAtBatchesInterleavedTileFrames(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: dbPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	a := ChunkCoord{X: -7, Y: -6}
	b := ChunkCoord{X: -8, Y: -6}
	firstA := make([]uint16, ChunkPixelCount)
	firstB := make([]uint16, ChunkPixelCount)
	for i := range firstA {
		firstA[i] = 0x1001
		firstB[i] = 0x2002
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 10, Chunk: a, Pixels: firstA}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 11, Chunk: b, Pixels: firstB}); err != nil {
		t.Fatal(err)
	}
	secondA := append([]uint16(nil), firstA...)
	secondA[3] = 0x3003
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 12, Chunk: a, Pixels: secondA}); err != nil {
		t.Fatal(err)
	}
	secondB := append([]uint16(nil), firstB...)
	secondB[5] = 0x4004
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 13, Chunk: b, Pixels: secondB}); err != nil {
		t.Fatal(err)
	}
	thirdA := append([]uint16(nil), secondA...)
	thirdA[7] = 0x5005
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 14, Chunk: a, Pixels: thirdA}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	chunks, err := reopened.ChunksAt(ctx, key, 13, []ChunkCoord{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if got := chunks[a]; got == nil || got.Pixels[3] != 0x3003 || got.Pixels[7] != 0x1001 {
		t.Fatalf("chunk A at tick 13 = %+v", got)
	}
	if got := chunks[b]; got == nil || got.Pixels[5] != 0x4004 || got.Pixels[3] != 0x2002 {
		t.Fatalf("chunk B at tick 13 = %+v", got)
	}
}

func TestChunkSnapshotHeuristicBoundsReplay(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: dbPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	coord := ChunkCoord{X: -7, Y: -6}
	pixels := make([]uint16, ChunkPixelCount)
	for i := range pixels {
		pixels[i] = uint16(i)
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 1, Chunk: coord, Pixels: pixels}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < chunkSnapshotMaxDeltaFrames+8; i++ {
		pixels[i%ChunkPixelCount]++
		if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: uint64(i + 2), Chunk: coord, Pixels: pixels}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, indexes, err := readChunkTileIndex(chunkTileIndexPath(dbPath, key, chunkTileCoordForChunk(coord)))
	if err != nil {
		t.Fatal(err)
	}
	idx := indexes[coord]
	if len(idx.snapshots) < 2 {
		t.Fatalf("snapshot count = %d, want heuristic to add snapshots", len(idx.snapshots))
	}
	if replay := maxChunkReplayEvents(idx); replay > chunkSnapshotMaxDeltaEvents {
		t.Fatalf("max replay events = %d, want <= %d", replay, chunkSnapshotMaxDeltaEvents)
	}
	for i, snap := range idx.snapshots {
		end := uint32(len(idx.deltas))
		if i+1 < len(idx.snapshots) {
			end = idx.snapshots[i+1].FirstDeltaFrameIdx
		}
		if frames := end - snap.FirstDeltaFrameIdx; frames > chunkSnapshotMaxDeltaFrames {
			t.Fatalf("snapshot %d has %d delta frames, want <= %d", i, frames, chunkSnapshotMaxDeltaFrames)
		}
	}
}

func TestChunkStatsReloadedFromTileShardFormat(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: dbPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	first := make([]uint16, ChunkPixelCount)
	for i := range first {
		first[i] = 0x2462
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 10, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 11, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: first}); err != nil {
		t.Fatal(err)
	}
	second := append([]uint16(nil), first...)
	second[3] = 0xcdac
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 12, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: second}); err != nil {
		t.Fatal(err)
	}
	otherKey := DatastoreKey{SavefileUUID: "save-1", Surface: "vulcanus", Force: "player"}
	other := make([]uint16, ChunkPixelCount)
	for i := range other {
		other[i] = 0xf800
	}
	if _, err := db.IngestChunk(ctx, ChunkIngest{Key: otherKey, Tick: 20, Chunk: ChunkCoord{X: 0, Y: 0}, Pixels: other}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats, err := reopened.ChunkStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Format != FormatChunks || stats.DatastoreCount != 2 || stats.ChunkCount != 2 || stats.TileShardCount != 2 {
		t.Fatalf("stats summary = %+v", stats)
	}
	if stats.SnapshotCount != 2 || stats.DeltaFrameCount != 1 {
		t.Fatalf("stats frames snapshots=%d deltas=%d, want 2 snapshots and 1 delta", stats.SnapshotCount, stats.DeltaFrameCount)
	}
	if stats.StoredPixelEvents != 2*ChunkPixelCount+1 {
		t.Fatalf("stored pixel events = %d, want %d", stats.StoredPixelEvents, 2*ChunkPixelCount+1)
	}
	if stats.CompressedBytes == 0 || stats.AverageSnapshotBytes == 0 || stats.AverageDeltaFrameBytes == 0 {
		t.Fatalf("compressed byte stats not populated: %+v", stats)
	}
	if stats.MaxReplayEventsBetweenSnaps != 1 {
		t.Fatalf("max replay events = %d, want 1", stats.MaxReplayEventsBetweenSnaps)
	}
	if len(stats.Datastores) != 2 {
		t.Fatalf("datastore stats count = %d, want 2", len(stats.Datastores))
	}
	if stats.Datastores[0].Surface != "nauvis" || stats.Datastores[0].LatestRowSeq != 3 {
		t.Fatalf("first datastore stats = %+v", stats.Datastores[0])
	}
	if stats.Datastores[1].Surface != "vulcanus" || stats.Datastores[1].LatestRowSeq != 4 {
		t.Fatalf("second datastore stats = %+v", stats.Datastores[1])
	}
}

func repeatedRGB565Hex(color uint16) string {
	return strings.Repeat(fmt.Sprintf("%02x%02x", byte(color), byte(color>>8)), ChunkPixelCount)
}

func sliceToChunkPixels(in []uint16) [ChunkPixelCount]uint16 {
	var pixels [ChunkPixelCount]uint16
	copy(pixels[:], in)
	return pixels
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("path should not exist: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}
