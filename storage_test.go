package timeshadedb

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTileAtReplaysSameSecondInInputOrderAndDropsUnchanged(t *testing.T) {
	dir := t.TempDir()
	oldEvents, oldSpan := deltaFrameMaxEvents, deltaFrameMaxSpan
	deltaFrameMaxEvents, deltaFrameMaxSpan = 2, 3600
	defer func() {
		deltaFrameMaxEvents, deltaFrameMaxSpan = oldEvents, oldSpan
	}()

	db, err := Open(OpenOptions{Path: filepath.Join(dir, "db.tshd")})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(100, 500_000_000).UTC()
	if err := db.IngestPlacement(ts, 10, 10, RGB{R: 1, G: 2, B: 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestPlacement(ts, 10, 10, RGB{R: 1, G: 2, B: 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestPlacement(ts, 10, 10, RGB{R: 4, G: 5, B: 6}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(OpenOptions{Path: filepath.Join(dir, "db.tshd"), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	pos := 10*res.Width + 10
	if got, want := res.Pixels[pos], uint8(2); got != want {
		t.Fatalf("pixel id = %d, want %d", got, want)
	}
	if got, want := len(res.Palette), 2; got != want {
		t.Fatalf("palette size = %d, want %d", got, want)
	}
	stats := db.Stats()
	if got, want := stats.DeltaFrameCount, uint64(1); got != want {
		t.Fatalf("delta frames = %d, want %d", got, want)
	}
	if got, want := stats.ChangedRowsStored, uint64(2); got != want {
		t.Fatalf("changed rows = %d, want %d", got, want)
	}
	if got, want := stats.UnchangedRowsDiscarded, uint64(1); got != want {
		t.Fatalf("unchanged rows = %d, want %d", got, want)
	}
	if got, want := len(stats.Tiles), TileCount; got != want {
		t.Fatalf("tile stats count = %d, want %d", got, want)
	}
	if got, want := stats.Tiles[0].ChangedPlacementsStored, uint64(2); got != want {
		t.Fatalf("tile changed rows = %d, want %d", got, want)
	}
	if got, want := stats.Tiles[0].UnchangedPlacementsDiscarded, uint64(1); got != want {
		t.Fatalf("tile unchanged rows = %d, want %d", got, want)
	}
}

func TestTileAtHistoricalBeforeAndAfterDelta(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(OpenOptions{Path: filepath.Join(dir, "db.tshd")})
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Unix(100, 0).UTC()
	t2 := time.Unix(200, 0).UTC()
	if err := db.IngestPlacement(t1, 513, 513, RGB{R: 10}); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestPlacement(t2, 513, 513, RGB{G: 20}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(OpenOptions{Path: filepath.Join(dir, "db.tshd"), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: time.Unix(150, 0).UTC(), Tile: TileCoord{X: 1, Y: 1}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: time.Unix(250, 0).UTC(), Tile: TileCoord{X: 1, Y: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pos := 1*before.Width + 1
	if got, want := before.Pixels[pos], uint8(1); got != want {
		t.Fatalf("before pixel id = %d, want %d", got, want)
	}
	if got, want := after.Pixels[pos], uint8(2); got != want {
		t.Fatalf("after pixel id = %d, want %d", got, want)
	}
}

func TestImportAndVerifySmallCSV(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.csv.gzip")
	if err := writeGzipCSV(input); err != nil {
		t.Fatal(err)
	}
	db, err := Open(OpenOptions{Path: filepath.Join(dir, "db.tshd")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCSV(context.Background(), db, input); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(OpenOptions{Path: filepath.Join(dir, "db.tshd"), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := VerifyCSV(context.Background(), ro, input); err != nil {
		t.Fatal(err)
	}
}

func TestImportDoesNotSnapshotEverySparseDeltaFrame(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "sparse.csv.gzip")
	if err := writeSparseGzipCSV(input, 20); err != nil {
		t.Fatal(err)
	}
	db, err := Open(OpenOptions{Path: filepath.Join(dir, "db.tshd")})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := ImportCSV(context.Background(), db, input)
	if err != nil {
		t.Fatal(err)
	}
	if got, max := stats.Tiles[0].SnapshotCount, uint64(3); got > max {
		t.Fatalf("tile snapshot count = %d, want <= %d", got, max)
	}
	if got, want := stats.Tiles[0].DeltaFrameCount, uint64(10); got != want {
		t.Fatalf("tile delta frame count = %d, want %d", got, want)
	}
}

func TestWALRecoveryReplaysUnfinalizedEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.tshd")
	db, err := Open(OpenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(1234, 0).UTC()
	if err := db.IngestPlacement(ts, 7, 8, RGB{R: 9, G: 8, B: 7}); err != nil {
		t.Fatal(err)
	}
	for _, tile := range db.tiles {
		if tile == nil {
			continue
		}
		if tile.file != nil {
			_ = tile.file.Close()
		}
		if tile.wal != nil {
			_ = tile.wal.Close()
		}
	}

	recovered, err := Open(OpenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	res, err := ro.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	pos := 8*res.Width + 7
	if got, want := res.Pixels[pos], uint8(1); got != want {
		t.Fatalf("recovered pixel id = %d, want %d", got, want)
	}
}

func TestSnapshotCacheReturnsCallerOwnedPixelsAndSupportsParallelQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.tshd")
	db, err := Open(OpenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(5000, 0).UTC()
	if err := db.IngestPlacement(ts, 3, 4, RGB{R: 1, G: 1, B: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(OpenOptions{Path: path, ReadOnly: true, CacheSize: 2 * 512 * 512})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	pos := 4*first.Width + 3
	first.Pixels[pos] = 99
	second, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := second.Pixels[pos], uint8(1); got != want {
		t.Fatalf("cached query pixel = %d, want %d", got, want)
	}
	var wg sync.WaitGroup
	errs := make(chan error, TileCount)
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			wg.Add(1)
			go func(x, y int) {
				defer wg.Done()
				_, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: x, Y: y}})
				errs <- err
			}(x, y)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactPreservesHistoricalQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.tshd")
	db, err := Open(OpenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Unix(10, 0).UTC()
	t2 := time.Unix(20, 0).UTC()
	if err := db.IngestPlacement(t1, 12, 13, RGB{R: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestPlacement(t2, 12, 13, RGB{G: 2}); err != nil {
		t.Fatal(err)
	}
	stats, err := db.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FramesRewritten == 0 {
		t.Fatal("compaction rewrote no frames")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	after, err := ro.TileAt(context.Background(), TileAtOptions{Timestamp: t1, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	beforePos := 13*after.Width + 12
	if got, want := after.Pixels[beforePos], uint8(1); got != want {
		t.Fatalf("after compaction historical pixel = %d, want %d", got, want)
	}
	latest, err := ro.TileAt(context.Background(), TileAtOptions{Timestamp: t2, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := latest.Pixels[beforePos], uint8(2); got != want {
		t.Fatalf("after compaction latest pixel = %d, want %d", got, want)
	}
}

func writeGzipCSV(path string) error {
	data := []byte("timestamp,user_id,pixel_color,coordinate\n" +
		"2022-04-01 12:00:00.123 UTC,u1,#010203,\"1,1\"\n" +
		"2022-04-01 12:00:00.456 UTC,u2,#010203,\"1,1\"\n" +
		"2022-04-01T12:01:00Z,u3,#040506,\"513,513\"\n")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		return err
	}
	return gz.Close()
}

func writeSparseGzipCSV(path string, rows int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("timestamp,user_id,pixel_color,coordinate\n")); err != nil {
		return err
	}
	base := time.Unix(1648814400, 0).UTC()
	for i := 0; i < rows; i++ {
		line := fmt.Sprintf("%s,u%d,#010203,\"%d,%d\"\n", base.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05.000 UTC"), i, i, i)
		if _, err := gz.Write([]byte(line)); err != nil {
			return err
		}
	}
	return gz.Close()
}
