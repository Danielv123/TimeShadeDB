package timeshadedb

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"
)

// chunkOracle records every ingested row, so tests can compute the expected chunk
// state at any tick the way the former in-memory event log did: the last row at or
// before the tick wins.
type chunkOracle struct {
	rows map[ChunkCoord][]oracleRow
}

type oracleRow struct {
	tick   uint64
	pixels [ChunkPixelCount]uint16
}

func newChunkOracle() *chunkOracle {
	return &chunkOracle{rows: map[ChunkCoord][]oracleRow{}}
}

func (o *chunkOracle) add(rows []ParsedChunkRow) {
	for _, row := range rows {
		o.rows[row.Chunk] = append(o.rows[row.Chunk], oracleRow{tick: row.Tick, pixels: row.Pixels})
	}
}

func (o *chunkOracle) at(coord ChunkCoord, tick uint64) (*[ChunkPixelCount]uint16, bool) {
	var out *[ChunkPixelCount]uint16
	for i := range o.rows[coord] {
		row := &o.rows[coord][i]
		if row.tick > tick {
			break
		}
		out = &row.pixels
	}
	return out, out != nil
}

func heapAllocAfterGC() uint64 {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

var testPalette = []uint16{0x52AA, 0x4A69, 0x31A6, 0x8C51, 0x2945, 0x6B4D, 0xA534, 0x18C3}

func randomChunkPixels(rng *rand.Rand) [ChunkPixelCount]uint16 {
	var px [ChunkPixelCount]uint16
	for i := range px {
		px[i] = testPalette[rng.Intn(len(testPalette))]
	}
	return px
}

// ingestRandomHistory writes rounds of random changes to chunks spread over several
// tiles (negative coordinates included). Some rounds touch many pixels, so the
// snapshot heuristic adds snapshots; some rows repeat a tick or resend unchanged data.
func ingestRandomHistory(t *testing.T, db *DB, key DatastoreKey, oracle *chunkOracle, rng *rand.Rand, coords []ChunkCoord, rounds int, firstTick uint64) uint64 {
	t.Helper()
	ctx := context.Background()
	state := map[ChunkCoord][ChunkPixelCount]uint16{}
	tick := firstTick
	for r := 0; r < rounds; r++ {
		var rows []ParsedChunkRow
		for _, c := range coords {
			px, seen := state[c]
			switch {
			case !seen:
				px = randomChunkPixels(rng)
			case rng.Intn(3) == 0:
				continue
			default:
				changes := 1 + rng.Intn(8)
				if rng.Intn(10) == 0 {
					changes = 300 + rng.Intn(700)
				}
				for i := 0; i < changes; i++ {
					px[rng.Intn(ChunkPixelCount)] = testPalette[rng.Intn(len(testPalette))]
				}
			}
			state[c] = px
			rowTick := tick
			if rng.Intn(20) == 0 && rowTick > firstTick {
				rowTick-- // a second row at the previous round's tick
			}
			rows = append(rows, ParsedChunkRow{Key: key, Tick: rowTick, Chunk: c, Pixels: px})
		}
		if len(rows) == 0 {
			tick += 60
			continue
		}
		if _, err := db.IngestChunkRows(ctx, rows); err != nil {
			t.Fatal(err)
		}
		oracle.add(rows)
		tick += 60
	}
	return tick
}

func checkChunksAgainstOracle(t *testing.T, label string, db *DB, key DatastoreKey, oracle *chunkOracle, coords []ChunkCoord, ticks []uint64) {
	t.Helper()
	ctx := context.Background()
	for _, tick := range ticks {
		got, err := db.ChunksAt(ctx, key, tick, coords)
		if err != nil {
			t.Fatalf("%s: ChunksAt(%d): %v", label, tick, err)
		}
		for _, c := range coords {
			want, ok := oracle.at(c, tick)
			res := got[c]
			if !ok {
				if res != nil {
					t.Fatalf("%s: chunk %v at tick %d returned data, want none", label, c, tick)
				}
				continue
			}
			if res == nil {
				t.Fatalf("%s: chunk %v at tick %d missing", label, c, tick)
			}
			for i := range want {
				if res.Pixels[i] != want[i] {
					t.Fatalf("%s: chunk %v tick %d pixel %d = %#04x, want %#04x", label, c, tick, i, res.Pixels[i], want[i])
				}
			}
		}
	}
}

func TestChunkHistoryMatchesOracleAcrossReopenAndCopy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.tshd")
	db, err := Open(OpenOptions{Path: dbPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	coords := []ChunkCoord{{X: -17, Y: -1}, {X: -16, Y: -1}, {X: -1, Y: -1}, {X: 0, Y: 0}, {X: 3, Y: 15}, {X: 16, Y: 3}, {X: 40, Y: -33}}
	oracle := newChunkOracle()
	rng := rand.New(rand.NewSource(7))
	end := ingestRandomHistory(t, db, key, oracle, rng, coords, 400, 1000)
	ticks := []uint64{0, 999, 1000, 1001, 1059, 1060, end / 2, end/2 + 1, end - 61, end - 60, end, end + 1000}
	for i := 0; i < 40; i++ {
		ticks = append(ticks, 1000+uint64(rng.Int63n(int64(end-1000))))
	}
	checkChunksAgainstOracle(t, "live", db, key, oracle, coords, ticks)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	checkChunksAgainstOracle(t, "reopened", ro, key, oracle, coords, ticks)
	meta, err := ro.IngestMetadata("save-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Datastores) != 1 || meta.Datastores[0].LatestTick == 0 {
		t.Fatalf("metadata after reopen = %+v", meta)
	}

	copyPath := filepath.Join(dir, "copy.tshd")
	dst, err := Open(OpenOptions{Path: copyPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.CopyChunksTo(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	checkChunksAgainstOracle(t, "copy", dst, key, oracle, coords, ticks)
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	// Writing after a reopen continues the history: the restored live state is the
	// diff base, and the sequence numbers keep growing.
	rw, err := Open(OpenOptions{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	end2 := ingestRandomHistory(t, rw, key, oracle, rng, coords, 50, end)
	checkChunksAgainstOracle(t, "continued", rw, key, oracle, coords, append(ticks, end+30, end2))
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestChunksAtUnknownTilesRetainNoMemory is the stopgap for G2: reads of tiles that
// hold no chunk, in an existing datastore, must not leave anything behind.
func TestChunksAtUnknownTilesRetainNoMemory(t *testing.T) {
	ctx := context.Background()
	db, err := Open(OpenOptions{Path: filepath.Join(t.TempDir(), "db.tshd"), Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	rng := rand.New(rand.NewSource(2))
	if _, err := db.IngestChunkRows(ctx, []ParsedChunkRow{{Key: key, Tick: 1, Chunk: ChunkCoord{}, Pixels: randomChunkPixels(rng)}}); err != nil {
		t.Fatal(err)
	}
	// 200 requests of 64x64 chunks, each chunk in a distinct 512 px tile: 819,200
	// distinct tile keys, as a zoomed-out viewer (or a hostile client) would ask for.
	const grid = 64
	coords := make([]ChunkCoord, 0, grid*grid)
	before := heapAllocAfterGC()
	for req := 0; req < 200; req++ {
		coords = coords[:0]
		bx := int32(req * grid * 32)
		for y := 0; y < grid; y++ {
			for x := 0; x < grid; x++ {
				coords = append(coords, ChunkCoord{X: (bx + int32(x)) * 32, Y: int32(y) * 32})
			}
		}
		if _, err := db.ChunksAt(ctx, key, 100, coords); err != nil {
			t.Fatal(err)
		}
	}
	after := heapAllocAfterGC()
	grown := int64(after) - int64(before)
	t.Logf("heap before %d B, after 819200 unknown tile keys %d B (%+d B)", before, after, grown)
	if grown > 1<<20 {
		t.Fatalf("heap grew %d B for unknown tiles, want <= 1 MiB", grown)
	}

	// An unknown datastore is an error and creates nothing either.
	_, err = db.ChunksAt(ctx, DatastoreKey{SavefileUUID: "nope", Surface: "nauvis", Force: "player"}, 1, []ChunkCoord{{}})
	if !errors.Is(err, ErrDatastoreNotFound) {
		t.Fatalf("unknown datastore error = %v, want ErrDatastoreNotFound", err)
	}
	if _, ok, _ := db.ChunkDatastoreLatestTick(DatastoreKey{SavefileUUID: "nope", Surface: "nauvis", Force: "player"}); ok {
		t.Fatal("unknown datastore reported as existing")
	}
	if len(db.chunkDatastores) != 1 {
		t.Fatalf("datastores = %d, want 1", len(db.chunkDatastores))
	}
}

// TestChunkIngestHeapGrowth is the WP1 grow test: heap growth per ingested row must
// stay small now that history is no longer kept in memory. The default run is short;
// set TSDB_GROW_ROUNDS=1000 for the full acceptance run (about 25 s).
func TestChunkIngestHeapGrowth(t *testing.T) {
	rounds := 150
	if v := os.Getenv("TSDB_GROW_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		rounds = n
	} else if testing.Short() {
		t.Skip("short mode")
	}
	ctx := context.Background()
	db, err := Open(OpenOptions{Path: filepath.Join(t.TempDir(), "db.tshd"), Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := DatastoreKey{SavefileUUID: "exp", Surface: "nauvis", Force: "player"}
	rng := rand.New(rand.NewSource(1))
	const side = 48
	coords := make([]ChunkCoord, 0, side*side)
	state := map[ChunkCoord][ChunkPixelCount]uint16{}
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			c := ChunkCoord{X: int32(x - side/2), Y: int32(y - side/2)}
			coords = append(coords, c)
			state[c] = randomChunkPixels(rng)
		}
	}
	var rowsTotal uint64
	var startHeap uint64
	var startRows uint64
	warmup := rounds / 10
	for r := 0; r <= rounds; r++ {
		rows := make([]ParsedChunkRow, 0, len(coords))
		for _, c := range coords {
			px := state[c]
			if r > 0 {
				if rng.Float64() > 0.25 {
					continue
				}
				for i := 0; i < 8; i++ {
					px[rng.Intn(ChunkPixelCount)] = testPalette[rng.Intn(len(testPalette))]
				}
				state[c] = px
			}
			rows = append(rows, ParsedChunkRow{Key: key, Tick: uint64(r) * 3600, Chunk: c, Pixels: px})
		}
		for i := 0; i < len(rows); i += 4096 {
			res, err := db.IngestChunkRows(ctx, rows[i:min(i+4096, len(rows))])
			if err != nil {
				t.Fatal(err)
			}
			rowsTotal += res.AcceptedRows
		}
		if r == warmup {
			startHeap, startRows = heapAllocAfterGC(), rowsTotal
		}
	}
	endHeap := heapAllocAfterGC()
	slope := (float64(endHeap) - float64(startHeap)) / float64(rowsTotal-startRows)
	t.Logf("rounds=%d rows=%d heap %.1f MB -> %.1f MB, slope %.1f B/row", rounds, rowsTotal, float64(startHeap)/(1<<20), float64(endHeap)/(1<<20), slope)
	if endHeap > 400<<20 {
		t.Fatalf("heap %d B, want <= 400 MiB", endHeap)
	}
	if slope > 200 {
		t.Fatalf("heap slope %.1f B/row, want <= 200 B/row (0.2 KB)", slope)
	}
}

// writeLegacyChunkFile writes a legacy per-chunk .cdat/.cidx pair holding the given
// frames: a frame with nil changes is a snapshot of pixels, otherwise a delta.
func writeLegacyChunkFile(t *testing.T, dbPath string, key DatastoreKey, coord ChunkCoord, frames []legacyFrame) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(chunkDataPath(dbPath, key, coord)), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(chunkDataPath(dbPath, key, coord), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := writeChunkDataHeader(f, coord); err != nil {
		t.Fatal(err)
	}
	writer := &DB{compressor: newCompressionPool()}
	defer writer.compressor.close()
	var idx chunkIndex
	for _, frame := range frames {
		if frame.changes == nil {
			raw := encodeChunkSnapshot(frame.pixels)
			offset, compLen, checksum, err := writer.writeChunkFrame(f, frameKindSnapshot, frame.tick, ChunkPixelCount, raw)
			if err != nil {
				t.Fatal(err)
			}
			idx.snapshots = append(idx.snapshots, chunkSnapshotRecord{Tick: frame.tick, FrameOffset: offset, FrameHeaderLen: chunkFrameHeaderLen, CompressedLen: compLen, RawLen: uint32(len(raw)), FirstDeltaFrameIdx: uint32(len(idx.deltas)), EventSeq: frame.seq, Checksum: checksum})
			continue
		}
		raw := encodeChunkDeltaPayload(frame.tick, frame.changes)
		offset, compLen, checksum, err := writer.writeChunkFrame(f, frameKindDelta, frame.tick, uint32(len(frame.changes)), raw)
		if err != nil {
			t.Fatal(err)
		}
		idx.deltas = append(idx.deltas, chunkDeltaRecord{MinTick: frame.tick, MaxTick: frame.tick, FrameOffset: offset, FrameHeaderLen: chunkFrameHeaderLen, CompressedLen: compLen, RawLen: uint32(len(raw)), EventCount: uint32(len(frame.changes)), FirstEventSeq: frame.seq, LastEventSeq: frame.seq, Checksum: checksum})
	}
	if err := writeChunkIndex(dbPath, key, coord, idx); err != nil {
		t.Fatal(err)
	}
}

type legacyFrame struct {
	tick    uint64
	seq     uint64
	pixels  [ChunkPixelCount]uint16
	changes []chunkPixelChange
}

// TestLegacyChunkHistoryStaysReadable covers legacy per-chunk files without the
// in-memory event log: a legacy history with snapshot and deltas, a legacy history
// that starts with deltas (replayed from black), and the switch to tile frames on the
// first write, before and after a reopen and through CopyChunksTo.
func TestLegacyChunkHistoryStaysReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.tshd")
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	if err := os.MkdirAll(chunkDatastorePath(dbPath, key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeChunkManifest(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := writeChunkMetadata(chunkDatastorePath(dbPath, key), key); err != nil {
		t.Fatal(err)
	}
	var base [ChunkPixelCount]uint16
	for i := range base {
		base[i] = 0x2462
	}
	a := ChunkCoord{X: -7, Y: -6}
	b := ChunkCoord{X: 2, Y: 3}
	writeLegacyChunkFile(t, dbPath, key, a, []legacyFrame{
		{tick: 10, seq: 1, pixels: base},
		{tick: 20, seq: 3, changes: []chunkPixelChange{{Pos: 0, Color: 0x1111}}},
		{tick: 30, seq: 5, changes: []chunkPixelChange{{Pos: 1, Color: 0x2222}}},
	})
	writeLegacyChunkFile(t, dbPath, key, b, []legacyFrame{
		{tick: 15, seq: 2, changes: []chunkPixelChange{{Pos: 5, Color: 0x5555}}},
		{tick: 25, seq: 4, changes: []chunkPixelChange{{Pos: 6, Color: 0x6666}}},
	})
	if err := writeChunkProgress(chunkDatastorePath(dbPath, key), chunkDatastoreProgress{LatestTick: 30, LatestChunkX: a.X, LatestChunkY: a.Y, LatestRowSeq: 5}); err != nil {
		t.Fatal(err)
	}

	type probe struct {
		coord ChunkCoord
		tick  uint64
		pos   int
		want  uint16
		none  bool
	}
	legacyProbes := []probe{
		{a, 9, 0, 0, true},
		{a, 10, 0, 0x2462, false},
		{a, 19, 0, 0x2462, false},
		{a, 20, 0, 0x1111, false},
		{a, 25, 1, 0x2462, false},
		{a, 30, 1, 0x2222, false},
		{b, 14, 5, 0, true},
		{b, 15, 5, 0x5555, false},
		{b, 15, 6, 0, false},
		{b, 24, 6, 0, false},
		{b, 25, 6, 0x6666, false},
		{b, 99, 0, 0, false},
	}
	check := func(label string, db *DB, probes []probe) {
		t.Helper()
		for _, p := range probes {
			got, err := db.ChunksAt(ctx, key, p.tick, []ChunkCoord{p.coord})
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			res := got[p.coord]
			if p.none {
				if res != nil {
					t.Fatalf("%s: chunk %v at tick %d has data, want none", label, p.coord, p.tick)
				}
				continue
			}
			if res == nil || res.Pixels[p.pos] != p.want {
				t.Fatalf("%s: chunk %v tick %d pixel %d = %v, want %#04x", label, p.coord, p.tick, p.pos, res, p.want)
			}
		}
	}
	ro, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	check("read-only", ro, legacyProbes)
	if latest, ok, err := ro.ChunkDatastoreLatestTick(key); err != nil || !ok || latest != 30 {
		t.Fatalf("latest tick = %d %v %v, want 30", latest, ok, err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	rw, err := Open(OpenOptions{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	next := base
	next[0], next[1], next[2] = 0x1111, 0x2222, 0x3333
	res, err := rw.IngestChunk(ctx, ChunkIngest{Key: key, Tick: 40, Chunk: a, Pixels: next[:]})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChangedPixels != 1 {
		t.Fatalf("changed pixels = %d, want 1 (diff against the restored legacy state)", res.ChangedPixels)
	}
	if res.LatestRowSeq != 6 {
		t.Fatalf("row seq = %d, want 6 (after the legacy frames)", res.LatestRowSeq)
	}
	afterWrite := append(legacyProbes, probe{a, 40, 2, 0x3333, false}, probe{a, 39, 2, 0x2462, false})
	check("after first tile write", rw, afterWrite)
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	check("reopened", reopened, afterWrite)
	copyPath := filepath.Join(dir, "copy.tshd")
	dst, err := Open(OpenOptions{Path: copyPath, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := reopened.CopyChunksTo(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Chunks != 2 || stats.RowsCopied != 7 {
		t.Fatalf("copy stats = %+v, want 2 chunks and 6 frames plus the progress row", stats)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	check("copy", dst, afterWrite)
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentChunkIngestAndReads runs ingest and reads on shared tiles at once. It
// checks that reads never see a state newer than what was asked for and that nothing
// deadlocks. (The race detector needs cgo, which is not available everywhere.)
func TestConcurrentChunkIngestAndReads(t *testing.T) {
	ctx := context.Background()
	db, err := Open(OpenOptions{Path: filepath.Join(t.TempDir(), "db.tshd"), Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	coords := []ChunkCoord{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 15, Y: 15}, {X: 16, Y: 0}, {X: -1, Y: -1}}
	// Pixel 0 of every row holds its tick, so a read can tell which row it saw.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	const writers = 4
	const rounds = 150
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 1; r <= rounds; r++ {
				tick := uint64(r*writers + w)
				var rows []ParsedChunkRow
				for _, c := range coords {
					var px [ChunkPixelCount]uint16
					px[0] = uint16(tick)
					px[1+w] = uint16(r)
					rows = append(rows, ParsedChunkRow{Key: key, Tick: tick, Chunk: c, Pixels: px})
				}
				if _, err := db.IngestChunkRows(ctx, rows); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for rd := 0; rd < 4; rd++ {
		wg.Add(1)
		go func(rd int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(rd)))
			for i := 0; i < 300; i++ {
				tick := uint64(rng.Intn(rounds*writers + writers))
				got, err := db.ChunksAt(ctx, key, tick, coords)
				if errors.Is(err, ErrDatastoreNotFound) {
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				for c, res := range got {
					if uint64(res.Pixels[0]) > tick && res.SnapshotTick <= tick {
						errs <- fmt.Errorf("chunk %v at tick %d shows row of tick %d", c, tick, res.Pixels[0])
						return
					}
				}
			}
		}(rd)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	latest, ok, err := db.ChunkDatastoreLatestTick(key)
	if err != nil || !ok || latest != uint64(rounds*writers+writers-1) {
		t.Fatalf("latest tick = %d %v %v", latest, ok, err)
	}
}
