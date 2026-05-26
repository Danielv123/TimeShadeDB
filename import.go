package timeshadedb

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ImportStats struct {
	TotalRows                   uint64      `json:"total_rows_seen"`
	ChangedRowsStored           uint64      `json:"changed_rows_stored"`
	UnchangedRowsDiscarded      uint64      `json:"unchanged_rows_discarded"`
	PaletteSize                 int         `json:"palette_size"`
	SnapshotCount               uint64      `json:"snapshot_count"`
	AverageSnapshotBytes        uint64      `json:"average_snapshot_compressed_bytes"`
	DeltaFrameCount             uint64      `json:"delta_frame_count"`
	AverageDeltaFrameBytes      uint64      `json:"average_delta_frame_compressed_bytes"`
	CompressedBytes             uint64      `json:"compressed_bytes"`
	MaxReplayEventsBetweenSnaps uint32      `json:"max_replay_events_between_snapshots"`
	Tiles                       []TileStats `json:"tiles"`
}

type VerifyStats struct {
	RowsChecked  uint64 `json:"rows_checked"`
	PaletteSize  int    `json:"palette_size"`
	TilesChecked int    `json:"tiles_checked"`
}

type TileStats struct {
	Tile                         TileCoord `json:"tile"`
	Width                        int       `json:"width"`
	Height                       int       `json:"height"`
	SnapshotCount                uint64    `json:"snapshot_count"`
	AverageSnapshotBytes         uint64    `json:"average_snapshot_compressed_bytes"`
	DeltaFrameCount              uint64    `json:"delta_frame_count"`
	AverageDeltaFrameBytes       uint64    `json:"average_delta_frame_compressed_bytes"`
	CompressedBytes              uint64    `json:"compressed_bytes"`
	MaxReplayEventsBetweenSnaps  uint32    `json:"max_replay_events_between_snapshots"`
	UnchangedPlacementsDiscarded uint64    `json:"unchanged_placements_discarded"`
	ChangedPlacementsStored      uint64    `json:"changed_placements_stored"`
}

type csvParseJob struct {
	seq uint64
	row uint64
	rec []string
}

type parsedPlacement struct {
	seq uint64
	row uint64
	ts  time.Time
	x   int
	y   int
	rgb RGB
	err error
}

func ImportCSV(ctx context.Context, db *DB, inputPath string) (*ImportStats, error) {
	db.skipWAL = true
	defer func() {
		db.skipWAL = false
	}()
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	if len(header) < 4 || header[0] != "timestamp" || header[2] != "pixel_color" || header[3] != "coordinate" {
		return nil, fmt.Errorf("timeshadedb: unexpected CSV header: %v", header)
	}
	rows, err := importCSVRows(ctx, db, r)
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	stats := db.Stats()
	stats.TotalRows = rows
	return stats, nil
}

func importCSVRows(ctx context.Context, db *DB, r *csv.Reader) (uint64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan csvParseJob, 2*workers)
	parsed := make(chan parsedPlacement, 2*workers)
	sourceErr := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for job := range jobs {
				p := parseCSVJob(job)
				select {
				case parsed <- p:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(parsed)
	}()
	go func() {
		defer close(jobs)
		var seq uint64
		for {
			if err := ctx.Err(); err != nil {
				sourceErr <- err
				return
			}
			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				sourceErr <- nil
				return
			}
			if err != nil {
				sourceErr <- err
				return
			}
			job := csvParseJob{seq: seq, row: seq + 2, rec: rec}
			select {
			case jobs <- job:
				seq++
			case <-ctx.Done():
				sourceErr <- ctx.Err()
				return
			}
		}
	}()

	pending := map[uint64]parsedPlacement{}
	tileQueues, tileDone := startImportTileWorkers(ctx, db)
	tileQueuesClosed := false
	closeQueues := func() {
		if !tileQueuesClosed {
			closeTileQueues(tileQueues)
			tileQueuesClosed = true
		}
	}
	defer closeQueues()
	var next uint64
	for p := range parsed {
		if p.err != nil {
			cancel()
			return next, p.err
		}
		pending[p.seq] = p
		for {
			ready, ok := pending[next]
			if !ok {
				break
			}
			if ready.x < 0 || ready.x >= CanvasWidth || ready.y < 0 || ready.y >= CanvasHeight {
				cancel()
				return next, fmt.Errorf("row %d ingest: timeshadedb: coordinate out of bounds: %d,%d", ready.row, ready.x, ready.y)
			}
			c, err := db.paletteID(ready.rgb)
			if err != nil {
				cancel()
				return next, fmt.Errorf("row %d ingest: %w", ready.row, err)
			}
			db.totalRows++
			placement := tilePlacement{
				sec: unixSec(ready.ts),
				x:   ready.x,
				y:   ready.y,
				c:   c,
				seq: db.nextEventSeq(),
			}
			queue := tileQueues[tileSlot(ready.x/TileSize, ready.y/TileSize)]
			select {
			case queue <- placement:
			case <-ctx.Done():
				cancel()
				return next, ctx.Err()
			}
			delete(pending, next)
			next++
		}
	}
	closeQueues()
	if err := tileDone(); err != nil {
		cancel()
		return next, err
	}
	if err := <-sourceErr; err != nil {
		return next, err
	}
	if len(pending) != 0 {
		return next, errors.New("timeshadedb: parser pipeline ended with out-of-order rows pending")
	}
	return next, nil
}

func startImportTileWorkers(ctx context.Context, db *DB) ([TileCount]chan tilePlacement, func() error) {
	var queues [TileCount]chan tilePlacement
	errCh := make(chan error, TileCount)
	var wg sync.WaitGroup
	for i, t := range db.tiles {
		queues[i] = make(chan tilePlacement, 1024)
		wg.Add(1)
		go func(t *tileState, in <-chan tilePlacement) {
			defer wg.Done()
			for p := range in {
				if err := ctx.Err(); err != nil {
					errCh <- err
					return
				}
				if err := db.ingestTilePlacement(p, false); err != nil {
					errCh <- err
					return
				}
			}
			if err := db.flushDelta(t); err != nil {
				errCh <- err
			}
		}(t, queues[i])
	}
	return queues, func() error {
		wg.Wait()
		close(errCh)
		return errors.Join(drainErrors(errCh)...)
	}
}

func closeTileQueues(queues [TileCount]chan tilePlacement) {
	for _, queue := range queues {
		if queue != nil {
			close(queue)
		}
	}
}

func drainErrors(errCh <-chan error) []error {
	var errs []error
	for err := range errCh {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func parseCSVJob(job csvParseJob) parsedPlacement {
	rec := job.rec
	if len(rec) < 4 {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("timeshadedb: row %d has %d fields", job.row, len(rec))}
	}
	ts, err := parseTimestamp(rec[0])
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d timestamp: %w", job.row, err)}
	}
	rgb, err := parseRGB(rec[2])
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d color: %w", job.row, err)}
	}
	x, y, err := parseCoord(rec[3])
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d coordinate: %w", job.row, err)}
	}
	return parsedPlacement{seq: job.seq, row: job.row, ts: ts, x: x, y: y, rgb: rgb}
}

func VerifyCSV(ctx context.Context, db *DB, inputPath string) (*VerifyStats, error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	naive := make([][]uint8, TileCount)
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			t := newTileState(x, y)
			naive[tileSlot(x, y)] = make([]uint8, t.w*t.h)
		}
	}
	var maxTime time.Time
	var rows uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		ts, err := parseTimestamp(rec[0])
		if err != nil {
			return nil, err
		}
		rgb, err := parseRGB(rec[2])
		if err != nil {
			return nil, err
		}
		id, ok := db.paletteMap[rgbKey(rgb)]
		if !ok {
			return nil, fmt.Errorf("timeshadedb: color %02x%02x%02x missing from database palette", rgb.R, rgb.G, rgb.B)
		}
		x, y, err := parseCoord(rec[3])
		if err != nil {
			return nil, err
		}
		tx, ty := x/TileSize, y/TileSize
		t := db.tiles[tileSlot(tx, ty)]
		pos := (y%TileSize)*t.w + (x % TileSize)
		naive[tileSlot(tx, ty)][pos] = id
		if ts.After(maxTime) {
			maxTime = ts
		}
		rows++
	}
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			res, err := db.TileAt(ctx, TileAtOptions{Timestamp: maxTime, Tile: TileCoord{X: x, Y: y}})
			if err != nil {
				return nil, err
			}
			want := naive[tileSlot(x, y)]
			if len(res.Pixels) != len(want) {
				return nil, fmt.Errorf("timeshadedb: tile %d,%d length mismatch: got %d want %d", x, y, len(res.Pixels), len(want))
			}
			for i := range want {
				if res.Pixels[i] != want[i] {
					return nil, fmt.Errorf("timeshadedb: tile %d,%d pixel %d mismatch: got %d want %d", x, y, i, res.Pixels[i], want[i])
				}
			}
		}
	}
	return &VerifyStats{RowsChecked: rows, PaletteSize: len(db.palette), TilesChecked: TileCount}, nil
}

func (db *DB) Stats() *ImportStats {
	stats := &ImportStats{PaletteSize: len(db.palette), TotalRows: db.totalRows}
	if stats.TotalRows == 0 && db.persistedStats != nil {
		stats.TotalRows = db.persistedStats.TotalRows
	}
	var liveUnchanged uint64
	stats.Tiles = make([]TileStats, 0, TileCount)
	for _, t := range db.tiles {
		if t == nil {
			continue
		}
		tileStats := TileStats{
			Tile:   TileCoord{X: t.x, Y: t.y},
			Width:  t.w,
			Height: t.h,
		}
		liveUnchanged += t.unchangedPlacements
		tileStats.UnchangedPlacementsDiscarded = t.unchangedPlacements
		tileStats.SnapshotCount = uint64(len(t.index.snapshots))
		tileStats.DeltaFrameCount = uint64(len(t.index.deltas))
		stats.SnapshotCount += tileStats.SnapshotCount
		stats.DeltaFrameCount += tileStats.DeltaFrameCount
		var tileSnapshotBytes uint64
		var tileDeltaBytes uint64
		for _, d := range t.index.deltas {
			tileStats.ChangedPlacementsStored += uint64(d.EventCount)
			tileDeltaBytes += uint64(d.CompressedLen)
		}
		for _, s := range t.index.snapshots {
			tileSnapshotBytes += uint64(s.CompressedLen)
		}
		tileStats.CompressedBytes = tileSnapshotBytes + tileDeltaBytes
		stats.ChangedRowsStored += tileStats.ChangedPlacementsStored
		stats.CompressedBytes += tileStats.CompressedBytes
		if tileStats.SnapshotCount > 0 {
			tileStats.AverageSnapshotBytes = tileSnapshotBytes / tileStats.SnapshotCount
		}
		if tileStats.DeltaFrameCount > 0 {
			tileStats.AverageDeltaFrameBytes = tileDeltaBytes / tileStats.DeltaFrameCount
		}
		for i, snap := range t.index.snapshots {
			end := uint32(len(t.index.deltas))
			if i+1 < len(t.index.snapshots) {
				end = t.index.snapshots[i+1].FirstDeltaFrameIdx
			}
			var replay uint32
			for di := snap.FirstDeltaFrameIdx; di < end; di++ {
				replay += t.index.deltas[di].EventCount
			}
			if replay > tileStats.MaxReplayEventsBetweenSnaps {
				tileStats.MaxReplayEventsBetweenSnaps = replay
			}
		}
		if tileStats.MaxReplayEventsBetweenSnaps > stats.MaxReplayEventsBetweenSnaps {
			stats.MaxReplayEventsBetweenSnaps = tileStats.MaxReplayEventsBetweenSnaps
		}
		stats.Tiles = append(stats.Tiles, tileStats)
	}
	stats.UnchangedRowsDiscarded = liveUnchanged
	if db.readOnly && db.persistedStats != nil {
		stats.UnchangedRowsDiscarded = db.persistedStats.UnchangedRowsDiscarded
		for i := range stats.Tiles {
			if i < len(db.persistedStats.Tiles) {
				stats.Tiles[i].UnchangedPlacementsDiscarded = db.persistedStats.Tiles[i].UnchangedPlacementsDiscarded
			}
		}
	} else if db.persistedStats != nil && db.persistedStats.UnchangedRowsDiscarded > stats.UnchangedRowsDiscarded {
		stats.UnchangedRowsDiscarded = db.persistedStats.UnchangedRowsDiscarded + liveUnchanged
	}
	if stats.SnapshotCount > 0 {
		var snapshotBytes uint64
		for _, tile := range stats.Tiles {
			snapshotBytes += tile.AverageSnapshotBytes * tile.SnapshotCount
		}
		stats.AverageSnapshotBytes = snapshotBytes / stats.SnapshotCount
	}
	if stats.DeltaFrameCount > 0 {
		var deltaBytes uint64
		for _, tile := range stats.Tiles {
			deltaBytes += tile.AverageDeltaFrameBytes * tile.DeltaFrameCount
		}
		stats.AverageDeltaFrameBytes = deltaBytes / stats.DeltaFrameCount
	}
	if stats.TotalRows == 0 {
		stats.TotalRows = stats.ChangedRowsStored + stats.UnchangedRowsDiscarded
	}
	return stats
}

func (db *DB) writeStats() error {
	stats := db.Stats()
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	db.persistedStats = stats
	return os.WriteFile(statsPath(db.path), data, 0o644)
}

func readStats(path string) (*ImportStats, error) {
	data, err := os.ReadFile(statsPath(path))
	if err != nil {
		return nil, err
	}
	var stats ImportStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	layouts := []string{
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05.999 MST",
		"2006-01-02 15:04:05.999999 MST",
		"2006-01-02 15:04:05.999999999 MST",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp format %q", s)
}

func parseRGB(s string) (RGB, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return RGB{}, fmt.Errorf("expected #RRGGBB, got %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return RGB{}, err
	}
	return RGB{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}, nil
}

func parseCoord(s string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected x,y, got %q", s)
	}
	x, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	y, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}
