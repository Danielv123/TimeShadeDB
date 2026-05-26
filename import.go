package timeshadedb

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	gzip "github.com/klauspost/pgzip"
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

type ImportOptions struct {
	MaxRows uint64
}

type VerifyOptions struct {
	MaxRows uint64
}

const (
	gzipReadBlockSize   = 1 << 20
	gzipReadAheadBlocks = 16
)

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
	seq  uint64
	row  uint64
	line string
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
	return ImportCSVWithOptions(ctx, db, inputPath, ImportOptions{})
}

func ImportCSVWithOptions(ctx context.Context, db *DB, inputPath string, opts ImportOptions) (*ImportStats, error) {
	db.skipWAL = true
	db.batchMode = true
	defer func() {
		db.skipWAL = false
		db.batchMode = false
	}()
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReaderN(f, gzipReadBlockSize, gzipReadAheadBlocks)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := bufio.NewReaderSize(gz, 1<<20)
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if strings.TrimRight(header, "\r\n") != "timestamp,user_id,pixel_color,coordinate" {
		return nil, fmt.Errorf("timeshadedb: unexpected CSV header: %q", strings.TrimRight(header, "\r\n"))
	}
	rows, err := importCSVRows(ctx, db, r, opts)
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

func importCSVRows(ctx context.Context, db *DB, r *bufio.Reader, opts ImportOptions) (uint64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tileQueues, tileDone := startImportTileWorkers(ctx, db)
	tileQueuesClosed := false
	closeQueues := func() {
		if !tileQueuesClosed {
			closeTileQueues(tileQueues)
			tileQueuesClosed = true
		}
	}
	defer closeQueues()
	var seq uint64
	for {
		if err := ctx.Err(); err != nil {
			cancel()
			return seq, err
		}
		if opts.MaxRows > 0 && seq >= opts.MaxRows {
			break
		}
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			cancel()
			return seq, err
		}
		if errors.Is(err, io.EOF) && line == "" {
			break
		}
		p := parseCSVJob(csvParseJob{seq: seq, row: seq + 2, line: line})
		if p.err != nil {
			cancel()
			return seq, p.err
		}
		if p.x < 0 || p.x >= CanvasWidth || p.y < 0 || p.y >= CanvasHeight {
			cancel()
			return seq, fmt.Errorf("row %d ingest: timeshadedb: coordinate out of bounds: %d,%d", p.row, p.x, p.y)
		}
		c, err := db.paletteID(p.rgb)
		if err != nil {
			cancel()
			return seq, fmt.Errorf("row %d ingest: %w", p.row, err)
		}
		db.totalRows++
		placement := tilePlacement{
			sec: unixSec(p.ts),
			x:   p.x,
			y:   p.y,
			c:   c,
			seq: db.nextEventSeq(),
		}
		queue := tileQueues[tileSlot(p.x/TileSize, p.y/TileSize)]
		select {
		case queue <- placement:
		case <-ctx.Done():
			cancel()
			return seq, ctx.Err()
		}
		seq++
		if errors.Is(err, io.EOF) {
			break
		}
	}
	closeQueues()
	if err := tileDone(); err != nil {
		cancel()
		return seq, err
	}
	return seq, nil
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
	tsText, colorText, coordText, err := splitPlacementLine(job.line)
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d: %w", job.row, err)}
	}
	ts, err := parseTimestamp(tsText)
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d timestamp: %w", job.row, err)}
	}
	rgb, err := parseRGB(colorText)
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d color: %w", job.row, err)}
	}
	x, y, err := parseCoord(coordText)
	if err != nil {
		return parsedPlacement{seq: job.seq, row: job.row, err: fmt.Errorf("row %d coordinate: %w", job.row, err)}
	}
	return parsedPlacement{seq: job.seq, row: job.row, ts: ts, x: x, y: y, rgb: rgb}
}

func splitPlacementLine(line string) (string, string, string, error) {
	line = strings.TrimRight(line, "\r\n")
	first := strings.IndexByte(line, ',')
	if first < 0 {
		return "", "", "", fmt.Errorf("malformed placement row")
	}
	secondRel := strings.IndexByte(line[first+1:], ',')
	if secondRel < 0 {
		return "", "", "", fmt.Errorf("malformed placement row")
	}
	second := first + 1 + secondRel
	thirdRel := strings.IndexByte(line[second+1:], ',')
	if thirdRel < 0 {
		return "", "", "", fmt.Errorf("malformed placement row")
	}
	third := second + 1 + thirdRel
	return line[:first], line[second+1 : third], line[third+1:], nil
}

func VerifyCSV(ctx context.Context, db *DB, inputPath string) (*VerifyStats, error) {
	return VerifyCSVWithOptions(ctx, db, inputPath, VerifyOptions{})
}

func VerifyCSVWithOptions(ctx context.Context, db *DB, inputPath string, opts VerifyOptions) (*VerifyStats, error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReaderN(f, gzipReadBlockSize, gzipReadAheadBlocks)
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
		if opts.MaxRows > 0 && rows >= opts.MaxRows {
			break
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
	if t, ok := parseFixedUTCTimestamp(s); ok {
		return t, nil
	}
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

func parseFixedUTCTimestamp(s string) (time.Time, bool) {
	if len(s) < len("2006-01-02 15:04:05") {
		return time.Time{}, false
	}
	if s[4] != '-' || s[7] != '-' || (s[10] != ' ' && s[10] != 'T') || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	if s[10] == 'T' && !strings.HasSuffix(s, "Z") {
		return time.Time{}, false
	}
	if s[10] == ' ' && !strings.HasSuffix(s, "UTC") {
		return time.Time{}, false
	}
	year, ok := parseFixedUint(s, 0, 4)
	if !ok {
		return time.Time{}, false
	}
	month, ok := parseFixedUint(s, 5, 2)
	if !ok {
		return time.Time{}, false
	}
	day, ok := parseFixedUint(s, 8, 2)
	if !ok {
		return time.Time{}, false
	}
	hour, ok := parseFixedUint(s, 11, 2)
	if !ok {
		return time.Time{}, false
	}
	min, ok := parseFixedUint(s, 14, 2)
	if !ok {
		return time.Time{}, false
	}
	sec, ok := parseFixedUint(s, 17, 2)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC), true
}

func parseFixedUint(s string, start, n int) (int, bool) {
	if len(s) < start+n {
		return 0, false
	}
	var v int
	for i := start; i < start+n; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	return v, true
}

func parseRGB(s string) (RGB, error) {
	if rgb, ok := parseRGBFast(s); ok {
		return rgb, nil
	}
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
	if x, y, ok := parseCoordFast(s); ok {
		return x, y, nil
	}
	s = strings.Trim(strings.TrimSpace(s), "\"")
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

func parseRGBFast(s string) (RGB, bool) {
	start, end := trimSpaceRange(s)
	s = s[start:end]
	if len(s) != 7 || s[0] != '#' {
		return RGB{}, false
	}
	r, ok := parseHexByte(s[1], s[2])
	if !ok {
		return RGB{}, false
	}
	g, ok := parseHexByte(s[3], s[4])
	if !ok {
		return RGB{}, false
	}
	b, ok := parseHexByte(s[5], s[6])
	if !ok {
		return RGB{}, false
	}
	return RGB{R: r, G: g, B: b}, true
}

func parseHexByte(hi, lo byte) (uint8, bool) {
	h, ok := hexValue(hi)
	if !ok {
		return 0, false
	}
	l, ok := hexValue(lo)
	if !ok {
		return 0, false
	}
	return h<<4 | l, true
}

func hexValue(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func parseCoordFast(s string) (int, int, bool) {
	start, end := trimSpaceRange(s)
	if start >= end {
		return 0, 0, false
	}
	if s[start] == '"' {
		start++
	}
	if end > start && s[end-1] == '"' {
		end--
	}
	x, pos, ok := parseLeadingInt(s, start, end)
	if !ok || pos >= end || s[pos] != ',' {
		return 0, 0, false
	}
	y, pos, ok := parseLeadingInt(s, pos+1, end)
	if !ok || pos != end {
		return 0, 0, false
	}
	return x, y, true
}

func parseLeadingInt(s string, start, end int) (int, int, bool) {
	if start >= end {
		return 0, start, false
	}
	orig := start
	var v int
	for start < end {
		c := s[start]
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int(c-'0')
		start++
	}
	return v, start, start > orig
}

func trimSpaceRange(s string) (int, int) {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r' || s[end-1] == '\n') {
		end--
	}
	return start, end
}
