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
		t.Fatalf("combined pixels not decoded as big-endian hex RGB565")
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
	changed, err := db.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 12, Chunk: ChunkCoord{X: -7, Y: -6}, Pixels: second})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ChangedPixels != 1 || changed.UnchangedPixels != ChunkPixelCount-1 {
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
	meta, err := db.IngestMetadata("save-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Datastores) != 1 || meta.Datastores[0].LatestTick != 12 || meta.Datastores[0].LatestChunkX != -7 {
		t.Fatalf("metadata = %+v", meta)
	}
	dataPath := chunkDataPath(dbPath, key, ChunkCoord{X: -7, Y: -6})
	indexPath := chunkIndexPath(dbPath, key, ChunkCoord{X: -7, Y: -6})
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("chunk data file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dbPath, "datastores")); err != nil {
		t.Fatalf("datastores directory missing: %v", err)
	}
	_, idx, err := readChunkIndex(indexPath)
	if err != nil {
		t.Fatalf("chunk index unreadable: %v", err)
	}
	if len(idx.snapshots) != 1 || len(idx.deltas) != 2 {
		t.Fatalf("chunk index snapshots=%d deltas=%d, want 1 snapshot and 2 deltas", len(idx.snapshots), len(idx.deltas))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	afterReload, err := reopened.ChunkAt(ctx, ChunkAtOptions{Key: key, Tick: 12, Chunk: ChunkCoord{X: -7, Y: -6}})
	if err != nil {
		t.Fatal(err)
	}
	if afterReload.Pixels[3] != 0xcdac {
		t.Fatalf("reloaded pixel = %#04x, want 0xcdac", afterReload.Pixels[3])
	}
}

func repeatedRGB565Hex(color uint16) string {
	return strings.Repeat(fmt.Sprintf("%04x", color), ChunkPixelCount)
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("path should not exist: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}
