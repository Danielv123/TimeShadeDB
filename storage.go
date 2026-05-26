package timeshadedb

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	formatVersion = uint16(1)

	frameKindSnapshot = uint8(1)
	frameKindDelta    = uint8(2)

	frameHeaderLen = uint16(29)

	defaultDeltaFrameMaxEvents = 16384
	defaultDeltaFrameMaxSpan   = 60
	minSnapshotSizeRuleBytes   = 64 * 1024
	timeSnapshotMinChanges     = 4096
)

var (
	deltaFrameMaxEvents = defaultDeltaFrameMaxEvents
	deltaFrameMaxSpan   = uint32(defaultDeltaFrameMaxSpan)
)

type manifest struct {
	Format        string `json:"format"`
	Version       int    `json:"version"`
	CanvasWidth   int    `json:"canvas_width"`
	CanvasHeight  int    `json:"canvas_height"`
	TileSize      int    `json:"tile_size"`
	TimestampUnit string `json:"timestamp_unit"`
	PaletteFile   string `json:"palette_file"`
	Codec         string `json:"codec"`
	CodecLevel    int    `json:"codec_level"`
}

type tileState struct {
	x int
	y int
	w int
	h int

	pixels []uint8
	file   *os.File
	wal    *os.File
	index  tileIndex

	builder []deltaEvent

	lastSnapshotSec     uint32
	changesSinceSnap    int
	deltaBytesSinceSnap int64
	lastSnapshotBytes   int64

	changedPlacements   uint64
	unchangedPlacements uint64
}

type tileIndex struct {
	snapshots []snapshotRecord
	deltas    []deltaRecord
}

type snapshotRecord struct {
	TimestampSec       uint32
	FrameOffset        uint64
	FrameHeaderLen     uint16
	CompressedLen      uint32
	RawLen             uint32
	FirstDeltaFrameIdx uint32
	EventSeq           uint64
	Checksum           uint32
}

type deltaRecord struct {
	MinTimestampSec uint32
	MaxTimestampSec uint32
	FrameOffset     uint64
	FrameHeaderLen  uint16
	CompressedLen   uint32
	RawLen          uint32
	EventCount      uint32
	FirstEventSeq   uint64
	LastEventSeq    uint64
	Checksum        uint32
}

type deltaEvent struct {
	sec uint32
	pos uint32
	c   uint8
	seq uint64
}

type tilePlacement struct {
	sec uint32
	x   int
	y   int
	c   uint8
	seq uint64
}

type frameInfo struct {
	kind          uint8
	timestampSec  uint32
	maxTimeSec    uint32
	eventCount    uint32
	rawLen        uint32
	compressedLen uint32
	checksum      uint32
	payload       []byte
}

type snapshotCacheKey struct {
	tileX  int
	tileY  int
	offset uint64
}

type snapshotCacheEntry struct {
	pixels []uint8
	bytes  int64
}

type compressionPool struct {
	jobs chan compressionJob
	wg   sync.WaitGroup
}

type compressionJob struct {
	raw  []byte
	kind uint8
	resp chan compressionResult
}

type compressionResult struct {
	payload []byte
	err     error
}

func newCompressionPool() *compressionPool {
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	p := &compressionPool{jobs: make(chan compressionJob, 2*workers)}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.wg.Done()
			fast, fastErr := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
			if fastErr == nil {
				defer fast.Close()
			}
			for job := range p.jobs {
				payload, err := compressWithFastEncoder(job.raw, job.kind, fast, fastErr)
				job.resp <- compressionResult{payload: payload, err: err}
			}
		}()
	}
	return p
}

func (p *compressionPool) compress(raw []byte, kind uint8) ([]byte, error) {
	if p == nil {
		return compressZstd(raw, kind)
	}
	resp := make(chan compressionResult, 1)
	p.jobs <- compressionJob{raw: raw, kind: kind, resp: resp}
	result := <-resp
	return result.payload, result.err
}

func (p *compressionPool) close() {
	if p == nil {
		return
	}
	close(p.jobs)
	p.wg.Wait()
}

func openDB(opts OpenOptions) (*DB, error) {
	if opts.Path == "" {
		return nil, errors.New("timeshadedb: OpenOptions.Path is required")
	}

	_, statErr := os.Stat(opts.Path)
	if errors.Is(statErr, os.ErrNotExist) {
		if opts.ReadOnly {
			return nil, fmt.Errorf("timeshadedb: database does not exist: %s", opts.Path)
		}
		return createDB(opts.Path, opts.CacheSize)
	}
	if statErr != nil {
		return nil, statErr
	}
	return loadDB(opts)
}

func createDB(path string, cacheSize int64) (*DB, error) {
	if err := os.MkdirAll(filepath.Join(path, "tiles"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(path, "wal"), 0o755); err != nil {
		return nil, err
	}
	db := &DB{
		path:          path,
		paletteMap:    map[uint32]uint8{},
		nextSeq:       1,
		compressor:    newCompressionPool(),
		cacheSize:     cacheSize,
		snapshotCache: map[snapshotCacheKey]*snapshotCacheEntry{},
	}
	if err := writeManifest(path); err != nil {
		return nil, err
	}
	if err := db.writePalette(); err != nil {
		return nil, err
	}
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			t, err := db.createTile(x, y)
			if err != nil {
				_ = db.close()
				return nil, err
			}
			db.tiles[tileSlot(x, y)] = t
			if err := db.writeSnapshot(t, 0); err != nil {
				_ = db.close()
				return nil, err
			}
			if err := db.writeTileIndex(t); err != nil {
				_ = db.close()
				return nil, err
			}
			if err := db.openTileWAL(t); err != nil {
				_ = db.close()
				return nil, err
			}
		}
	}
	return db, nil
}

func loadDB(opts OpenOptions) (*DB, error) {
	if err := readManifest(opts.Path); err != nil {
		return nil, err
	}
	db := &DB{
		path:          opts.Path,
		readOnly:      opts.ReadOnly,
		paletteMap:    map[uint32]uint8{},
		nextSeq:       1,
		cacheSize:     opts.CacheSize,
		snapshotCache: map[snapshotCacheKey]*snapshotCacheEntry{},
	}
	if !opts.ReadOnly {
		db.compressor = newCompressionPool()
	}
	if err := db.readPalette(); err != nil {
		return nil, err
	}
	if stats, err := readStats(opts.Path); err == nil {
		db.persistedStats = stats
		db.totalRows = stats.TotalRows
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			t := newTileState(x, y)
			idx, err := readTileIndex(opts.Path, t)
			if err != nil {
				_ = db.close()
				return nil, err
			}
			t.index = idx
			db.observeIndexSeq(idx)
			if len(idx.snapshots) > 0 {
				last := idx.snapshots[len(idx.snapshots)-1]
				t.lastSnapshotSec = last.TimestampSec
				t.lastSnapshotBytes = int64(last.CompressedLen)
			}
			flag := os.O_RDONLY
			if !opts.ReadOnly {
				flag = os.O_RDWR
			}
			f, err := os.OpenFile(tileDataPath(opts.Path, x, y), flag, 0)
			if err != nil {
				_ = db.close()
				return nil, err
			}
			t.file = f
			if !opts.ReadOnly {
				res, err := db.readTileAt(context.Background(), t, math.MaxUint32)
				if err != nil {
					_ = db.close()
					return nil, err
				}
				t.pixels = res.Pixels
				if _, err := t.file.Seek(0, io.SeekEnd); err != nil {
					_ = db.close()
					return nil, err
				}
				if err := db.replayTileWAL(t); err != nil {
					_ = db.close()
					return nil, err
				}
				if err := db.openTileWAL(t); err != nil {
					_ = db.close()
					return nil, err
				}
			}
			db.tiles[tileSlot(x, y)] = t
		}
	}
	return db, nil
}

func (db *DB) createTile(x, y int) (*tileState, error) {
	t := newTileState(x, y)
	t.pixels = make([]uint8, t.w*t.h)
	f, err := os.OpenFile(tileDataPath(db.path, x, y), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	t.file = f
	if err := writeTileDataHeader(f, x, y, t.w, t.h); err != nil {
		return nil, err
	}
	return t, nil
}

func newTileState(x, y int) *tileState {
	w := TileSize
	h := TileSize
	if x == TileCols-1 {
		w = CanvasWidth - x*TileSize
	}
	if y == TileRows-1 {
		h = CanvasHeight - y*TileSize
	}
	return &tileState{x: x, y: y, w: w, h: h}
}

func writeManifest(path string) error {
	m := manifest{
		Format:        "timeShadeDB",
		Version:       1,
		CanvasWidth:   CanvasWidth,
		CanvasHeight:  CanvasHeight,
		TileSize:      TileSize,
		TimestampUnit: "unix_second",
		PaletteFile:   "palette.bin",
		Codec:         "zstd",
		CodecLevel:    9,
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(path, "manifest.json"), data, 0o644)
}

func readManifest(path string) error {
	data, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m.Format != "timeShadeDB" || m.Version != 1 || m.CanvasWidth != CanvasWidth || m.CanvasHeight != CanvasHeight || m.TileSize != TileSize {
		return fmt.Errorf("timeshadedb: unsupported manifest in %s", path)
	}
	return nil
}

func (db *DB) ingestPlacement(ts time.Time, x, y int, rgb RGB) error {
	if db.closed {
		return errors.New("timeshadedb: database is closed")
	}
	if db.readOnly {
		return errors.New("timeshadedb: database opened read-only")
	}
	if x < 0 || x >= CanvasWidth || y < 0 || y >= CanvasHeight {
		return fmt.Errorf("timeshadedb: coordinate out of bounds: %d,%d", x, y)
	}
	db.totalRows++
	c, err := db.paletteID(rgb)
	if err != nil {
		return err
	}
	return db.ingestTilePlacement(tilePlacement{sec: unixSec(ts), x: x, y: y, c: c}, !db.skipWAL)
}

func (db *DB) ingestTilePlacement(p tilePlacement, writeWAL bool) error {
	if p.x < 0 || p.x >= CanvasWidth || p.y < 0 || p.y >= CanvasHeight {
		return fmt.Errorf("timeshadedb: coordinate out of bounds: %d,%d", p.x, p.y)
	}
	tx, ty := p.x/TileSize, p.y/TileSize
	t := db.tiles[tileSlot(tx, ty)]
	lx, ly := p.x%TileSize, p.y%TileSize
	pos := uint32(ly*t.w + lx)
	if t.pixels[pos] == p.c {
		t.unchangedPlacements++
		return nil
	}
	t.pixels[pos] = p.c
	if p.seq == 0 {
		p.seq = db.nextEventSeq()
	}
	ev := deltaEvent{sec: p.sec, pos: pos, c: p.c, seq: p.seq}
	if writeWAL {
		if err := appendWALEvent(t, ev); err != nil {
			return err
		}
	}
	t.builder = append(t.builder, ev)
	t.changedPlacements++
	t.changesSinceSnap++
	if shouldFlushDelta(t.builder) {
		return db.flushDelta(t)
	}
	return nil
}

func shouldFlushDelta(events []deltaEvent) bool {
	if len(events) == 0 {
		return false
	}
	if len(events) >= deltaFrameMaxEvents {
		return true
	}
	return events[len(events)-1].sec-events[0].sec >= deltaFrameMaxSpan
}

func (db *DB) nextEventSeq() uint64 {
	db.seqMu.Lock()
	defer db.seqMu.Unlock()
	seq := db.nextSeq
	db.nextSeq++
	return seq
}

func (db *DB) currentEventSeq() uint64 {
	db.seqMu.Lock()
	defer db.seqMu.Unlock()
	return db.nextSeq
}

func (db *DB) flushDelta(t *tileState) error {
	if len(t.builder) == 0 {
		return nil
	}
	events := append([]deltaEvent(nil), t.builder...)
	raw := encodeDeltaPayload(events)
	offset, compLen, checksum, err := db.writeFrame(t.file, frameKindDelta, events[0].sec, events[len(events)-1].sec, uint32(len(events)), raw)
	if err != nil {
		return err
	}
	t.index.deltas = append(t.index.deltas, deltaRecord{
		MinTimestampSec: events[0].sec,
		MaxTimestampSec: events[len(events)-1].sec,
		FrameOffset:     offset,
		FrameHeaderLen:  frameHeaderLen,
		CompressedLen:   compLen,
		RawLen:          uint32(len(raw)),
		EventCount:      uint32(len(events)),
		FirstEventSeq:   events[0].seq,
		LastEventSeq:    events[len(events)-1].seq,
		Checksum:        checksum,
	})
	t.builder = nil
	t.deltaBytesSinceSnap += int64(compLen)
	if db.shouldSnapshot(t, events[len(events)-1].sec) {
		if err := db.writeSnapshot(t, events[len(events)-1].sec); err != nil {
			return err
		}
	}
	if !db.batchMode {
		if err := db.writeTileIndex(t); err != nil {
			return err
		}
		if t.wal != nil {
			if err := db.clearTileWAL(t); err != nil {
				return err
			}
			return db.openTileWAL(t)
		}
	}
	return nil
}

func (db *DB) shouldSnapshot(t *tileState, sec uint32) bool {
	if t.changesSinceSnap == 0 {
		return false
	}
	pixels := t.w * t.h
	changeLimit := pixels / 4
	if changeLimit > 65536 {
		changeLimit = 65536
	}
	if t.changesSinceSnap >= changeLimit {
		return true
	}
	if t.changesSinceSnap >= timeSnapshotMinChanges && t.lastSnapshotBytes >= minSnapshotSizeRuleBytes && t.deltaBytesSinceSnap*4 >= t.lastSnapshotBytes*3 {
		return true
	}
	age := sec - t.lastSnapshotSec
	if age >= 15*60 && t.changesSinceSnap >= timeSnapshotMinChanges {
		return true
	}
	return age >= 60*60 && t.changesSinceSnap > 0
}

func (db *DB) writeSnapshot(t *tileState, sec uint32) error {
	raw := append([]byte(nil), t.pixels...)
	offset, compLen, checksum, err := db.writeFrame(t.file, frameKindSnapshot, sec, sec, 0, raw)
	if err != nil {
		return err
	}
	t.index.snapshots = append(t.index.snapshots, snapshotRecord{
		TimestampSec:       sec,
		FrameOffset:        offset,
		FrameHeaderLen:     frameHeaderLen,
		CompressedLen:      compLen,
		RawLen:             uint32(len(raw)),
		FirstDeltaFrameIdx: uint32(len(t.index.deltas)),
		EventSeq:           db.currentEventSeq(),
		Checksum:           checksum,
	})
	t.lastSnapshotSec = sec
	t.lastSnapshotBytes = int64(compLen)
	t.changesSinceSnap = 0
	t.deltaBytesSinceSnap = 0
	return nil
}

func (db *DB) writeFrame(f *os.File, kind uint8, timestampSec, maxTimeSec, eventCount uint32, raw []byte) (uint64, uint32, uint32, error) {
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, 0, err
	}
	payload, err := db.compressor.compress(raw, kind)
	if err != nil {
		return 0, 0, 0, err
	}
	checksum := crc32.ChecksumIEEE(raw)
	var hdr bytes.Buffer
	hdr.WriteString("TFRM")
	hdr.WriteByte(kind)
	for _, v := range []uint32{timestampSec, maxTimeSec, eventCount, uint32(len(raw)), uint32(len(payload)), checksum} {
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

func compressZstd(raw []byte, kind uint8) ([]byte, error) {
	level := zstd.SpeedBestCompression
	if kind == frameKindDelta && len(raw) < 4096 {
		level = zstd.SpeedFastest
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw, nil), nil
}

func compressWithFastEncoder(raw []byte, kind uint8, fast *zstd.Encoder, fastErr error) ([]byte, error) {
	if kind == frameKindDelta && len(raw) < 4096 {
		if fastErr != nil {
			return nil, fastErr
		}
		return fast.EncodeAll(raw, nil), nil
	}
	return compressZstd(raw, kind)
}

func decompressZstd(payload []byte, rawLen uint32) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(payload, nil)
	if err != nil {
		return nil, err
	}
	if len(raw) != int(rawLen) {
		return nil, fmt.Errorf("timeshadedb: decoded payload length %d, expected %d", len(raw), rawLen)
	}
	return raw, nil
}

func encodeDeltaPayload(events []deltaEvent) []byte {
	var buf bytes.Buffer
	buf.WriteString("TDEL")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(events)))
	base := events[0].sec
	_ = binary.Write(&buf, binary.LittleEndian, base)
	prev := base
	tmp := make([]byte, binary.MaxVarintLen64)
	for _, ev := range events {
		n := binary.PutUvarint(tmp, uint64(ev.sec-prev))
		buf.Write(tmp[:n])
		n = binary.PutUvarint(tmp, uint64(ev.pos))
		buf.Write(tmp[:n])
		buf.WriteByte(ev.c)
		prev = ev.sec
	}
	return buf.Bytes()
}

func decodeDeltaPayload(raw []byte) ([]deltaEvent, error) {
	if len(raw) < 14 || string(raw[:4]) != "TDEL" {
		return nil, errors.New("timeshadedb: bad delta payload")
	}
	version := binary.LittleEndian.Uint16(raw[4:6])
	if version != formatVersion {
		return nil, fmt.Errorf("timeshadedb: unsupported delta payload version %d", version)
	}
	count := binary.LittleEndian.Uint32(raw[6:10])
	base := binary.LittleEndian.Uint32(raw[10:14])
	events := make([]deltaEvent, 0, count)
	pos := 14
	sec := base
	for i := uint32(0); i < count; i++ {
		dt, n := binary.Uvarint(raw[pos:])
		if n <= 0 {
			return nil, errors.New("timeshadedb: bad delta timestamp varint")
		}
		pos += n
		p, n := binary.Uvarint(raw[pos:])
		if n <= 0 {
			return nil, errors.New("timeshadedb: bad delta position varint")
		}
		pos += n
		if pos >= len(raw) {
			return nil, errors.New("timeshadedb: truncated delta color")
		}
		sec += uint32(dt)
		events = append(events, deltaEvent{sec: sec, pos: uint32(p), c: raw[pos]})
		pos++
	}
	if pos != len(raw) {
		return nil, errors.New("timeshadedb: trailing bytes in delta payload")
	}
	return events, nil
}

func (db *DB) tileAt(ctx context.Context, opts TileAtOptions) (*TileResult, error) {
	if db.closed {
		return nil, errors.New("timeshadedb: database is closed")
	}
	if opts.Tile.X < 0 || opts.Tile.X >= TileCols || opts.Tile.Y < 0 || opts.Tile.Y >= TileRows {
		return nil, fmt.Errorf("timeshadedb: tile out of bounds: %d,%d", opts.Tile.X, opts.Tile.Y)
	}
	t := db.tiles[tileSlot(opts.Tile.X, opts.Tile.Y)]
	return db.readTileAt(ctx, t, unixSec(opts.Timestamp))
}

func (db *DB) readTileAt(ctx context.Context, t *tileState, sec uint32) (*TileResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snaps := t.index.snapshots
	if len(snaps) == 0 {
		return nil, errors.New("timeshadedb: tile has no snapshots")
	}
	i := sort.Search(len(snaps), func(i int) bool {
		return snaps[i].TimestampSec > sec
	}) - 1
	if i < 0 {
		i = 0
	}
	snap := snaps[i]
	pixels, err := db.loadSnapshotPixels(t, snap)
	if err != nil {
		return nil, err
	}
	if len(pixels) != t.w*t.h {
		return nil, fmt.Errorf("timeshadedb: snapshot has %d pixels, expected %d", len(pixels), t.w*t.h)
	}

	replayed := 0
	for di := int(snap.FirstDeltaFrameIdx); di < len(t.index.deltas); di++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := t.index.deltas[di]
		if d.MaxTimestampSec <= snap.TimestampSec {
			continue
		}
		if d.MinTimestampSec > sec {
			break
		}
		info, err := readFrame(t.file, d.FrameOffset)
		if err != nil {
			return nil, err
		}
		if info.kind != frameKindDelta {
			return nil, errors.New("timeshadedb: index pointed to non-delta frame")
		}
		raw, err := decompressZstd(info.payload, info.rawLen)
		if err != nil {
			return nil, err
		}
		if crc32.ChecksumIEEE(raw) != info.checksum || info.checksum != d.Checksum {
			return nil, errors.New("timeshadedb: delta checksum mismatch")
		}
		events, err := decodeDeltaPayload(raw)
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if ev.sec > sec {
				return tileResult(t, db.palette, pixels, snap.TimestampSec, replayed), nil
			}
			if int(ev.pos) >= len(pixels) {
				return nil, fmt.Errorf("timeshadedb: delta position out of bounds: %d", ev.pos)
			}
			pixels[ev.pos] = ev.c
			replayed++
		}
	}
	return tileResult(t, db.palette, pixels, snap.TimestampSec, replayed), nil
}

func tileResult(t *tileState, palette []RGB, pixels []uint8, snapSec uint32, replayed int) *TileResult {
	pal := append([]RGB(nil), palette...)
	return &TileResult{
		Tile:        TileCoord{X: t.x, Y: t.y},
		Width:       t.w,
		Height:      t.h,
		Palette:     pal,
		Pixels:      pixels,
		SnapshotSec: snapSec,
		Replayed:    replayed,
	}
}

func readFrame(f *os.File, offset uint64) (*frameInfo, error) {
	hdr := make([]byte, frameHeaderLen)
	if _, err := f.ReadAt(hdr, int64(offset)); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != "TFRM" {
		return nil, errors.New("timeshadedb: bad frame magic")
	}
	info := &frameInfo{
		kind:          hdr[4],
		timestampSec:  binary.LittleEndian.Uint32(hdr[5:9]),
		maxTimeSec:    binary.LittleEndian.Uint32(hdr[9:13]),
		eventCount:    binary.LittleEndian.Uint32(hdr[13:17]),
		rawLen:        binary.LittleEndian.Uint32(hdr[17:21]),
		compressedLen: binary.LittleEndian.Uint32(hdr[21:25]),
		checksum:      binary.LittleEndian.Uint32(hdr[25:29]),
	}
	info.payload = make([]byte, info.compressedLen)
	if _, err := f.ReadAt(info.payload, int64(offset)+int64(frameHeaderLen)); err != nil {
		return nil, err
	}
	return info, nil
}

func (db *DB) loadSnapshotPixels(t *tileState, snap snapshotRecord) ([]uint8, error) {
	key := snapshotCacheKey{tileX: t.x, tileY: t.y, offset: snap.FrameOffset}
	if db.cacheSize > 0 {
		db.cacheMu.Lock()
		if entry := db.snapshotCache[key]; entry != nil {
			pixels := append([]uint8(nil), entry.pixels...)
			db.cacheMu.Unlock()
			return pixels, nil
		}
		db.cacheMu.Unlock()
	}
	info, err := readFrame(t.file, snap.FrameOffset)
	if err != nil {
		return nil, err
	}
	if info.kind != frameKindSnapshot {
		return nil, errors.New("timeshadedb: index pointed to non-snapshot frame")
	}
	pixels, err := decompressZstd(info.payload, info.rawLen)
	if err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(pixels) != info.checksum || info.checksum != snap.Checksum {
		return nil, errors.New("timeshadedb: snapshot checksum mismatch")
	}
	db.storeSnapshotCache(key, pixels)
	return append([]uint8(nil), pixels...), nil
}

func (db *DB) storeSnapshotCache(key snapshotCacheKey, pixels []uint8) {
	if db.cacheSize <= 0 {
		return
	}
	size := int64(len(pixels))
	if size > db.cacheSize {
		return
	}
	db.cacheMu.Lock()
	defer db.cacheMu.Unlock()
	if old := db.snapshotCache[key]; old != nil {
		db.cacheBytes -= old.bytes
	} else {
		db.snapshotOrder = append(db.snapshotOrder, key)
	}
	db.snapshotCache[key] = &snapshotCacheEntry{pixels: append([]uint8(nil), pixels...), bytes: size}
	db.cacheBytes += size
	for db.cacheBytes > db.cacheSize && len(db.snapshotOrder) > 0 {
		victim := db.snapshotOrder[0]
		db.snapshotOrder = db.snapshotOrder[1:]
		entry := db.snapshotCache[victim]
		if entry == nil {
			continue
		}
		delete(db.snapshotCache, victim)
		db.cacheBytes -= entry.bytes
	}
}

func (db *DB) close() error {
	if db.closed {
		return nil
	}
	var errs []error
	if !db.readOnly {
		for _, t := range db.tiles {
			if t == nil {
				continue
			}
			if err := db.flushDelta(t); err != nil {
				errs = append(errs, err)
			}
			if err := db.writeTileIndex(t); err != nil {
				errs = append(errs, err)
			}
			if err := db.clearTileWAL(t); err != nil {
				errs = append(errs, err)
			}
		}
		if err := db.writePalette(); err != nil {
			errs = append(errs, err)
		}
		if err := db.writeStats(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, t := range db.tiles {
		if t != nil && t.file != nil {
			if err := t.file.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if t != nil && t.wal != nil {
			if err := t.wal.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if db.compressor != nil {
		db.compressor.close()
		db.compressor = nil
	}
	db.closed = true
	return errors.Join(errs...)
}

func (db *DB) writeTileIndex(t *tileState) error {
	if t.file == nil {
		return nil
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	size, err := t.file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("TIDX")
	for _, v := range []uint16{formatVersion, uint16(t.x), uint16(t.y), uint16(t.w), uint16(t.h)} {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	_ = binary.Write(&buf, binary.LittleEndian, uint64(size))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(t.index.snapshots)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(t.index.deltas)))
	for _, r := range t.index.snapshots {
		_ = binary.Write(&buf, binary.LittleEndian, r.TimestampSec)
		_ = binary.Write(&buf, binary.LittleEndian, r.FrameOffset)
		_ = binary.Write(&buf, binary.LittleEndian, r.FrameHeaderLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.CompressedLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.RawLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.FirstDeltaFrameIdx)
		_ = binary.Write(&buf, binary.LittleEndian, r.EventSeq)
		_ = binary.Write(&buf, binary.LittleEndian, r.Checksum)
	}
	for _, r := range t.index.deltas {
		_ = binary.Write(&buf, binary.LittleEndian, r.MinTimestampSec)
		_ = binary.Write(&buf, binary.LittleEndian, r.MaxTimestampSec)
		_ = binary.Write(&buf, binary.LittleEndian, r.FrameOffset)
		_ = binary.Write(&buf, binary.LittleEndian, r.FrameHeaderLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.CompressedLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.RawLen)
		_ = binary.Write(&buf, binary.LittleEndian, r.EventCount)
		_ = binary.Write(&buf, binary.LittleEndian, r.FirstEventSeq)
		_ = binary.Write(&buf, binary.LittleEndian, r.LastEventSeq)
		_ = binary.Write(&buf, binary.LittleEndian, r.Checksum)
	}
	tmp := tileIndexPath(db.path, t.x, t.y) + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, tileIndexPath(db.path, t.x, t.y))
}

func readTileIndex(path string, t *tileState) (tileIndex, error) {
	data, err := os.ReadFile(tileIndexPath(path, t.x, t.y))
	if err != nil {
		return tileIndex{}, err
	}
	r := bytes.NewReader(data)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return tileIndex{}, err
	}
	if string(magic) != "TIDX" {
		return tileIndex{}, errors.New("timeshadedb: bad index magic")
	}
	var version, x, y, w, h uint16
	for _, v := range []*uint16{&version, &x, &y, &w, &h} {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return tileIndex{}, err
		}
	}
	if version != formatVersion || int(x) != t.x || int(y) != t.y || int(w) != t.w || int(h) != t.h {
		return tileIndex{}, errors.New("timeshadedb: index header mismatch")
	}
	var dataSize uint64
	var snapCount, deltaCount uint32
	if err := binary.Read(r, binary.LittleEndian, &dataSize); err != nil {
		return tileIndex{}, err
	}
	if err := binary.Read(r, binary.LittleEndian, &snapCount); err != nil {
		return tileIndex{}, err
	}
	if err := binary.Read(r, binary.LittleEndian, &deltaCount); err != nil {
		return tileIndex{}, err
	}
	idx := tileIndex{
		snapshots: make([]snapshotRecord, snapCount),
		deltas:    make([]deltaRecord, deltaCount),
	}
	for i := range idx.snapshots {
		fields := []any{
			&idx.snapshots[i].TimestampSec,
			&idx.snapshots[i].FrameOffset,
			&idx.snapshots[i].FrameHeaderLen,
			&idx.snapshots[i].CompressedLen,
			&idx.snapshots[i].RawLen,
			&idx.snapshots[i].FirstDeltaFrameIdx,
			&idx.snapshots[i].EventSeq,
			&idx.snapshots[i].Checksum,
		}
		for _, f := range fields {
			if err := binary.Read(r, binary.LittleEndian, f); err != nil {
				return tileIndex{}, err
			}
		}
	}
	for i := range idx.deltas {
		fields := []any{
			&idx.deltas[i].MinTimestampSec,
			&idx.deltas[i].MaxTimestampSec,
			&idx.deltas[i].FrameOffset,
			&idx.deltas[i].FrameHeaderLen,
			&idx.deltas[i].CompressedLen,
			&idx.deltas[i].RawLen,
			&idx.deltas[i].EventCount,
			&idx.deltas[i].FirstEventSeq,
			&idx.deltas[i].LastEventSeq,
			&idx.deltas[i].Checksum,
		}
		for _, f := range fields {
			if err := binary.Read(r, binary.LittleEndian, f); err != nil {
				return tileIndex{}, err
			}
		}
	}
	if r.Len() != 0 {
		return tileIndex{}, errors.New("timeshadedb: trailing bytes in tile index")
	}
	return idx, nil
}

func (db *DB) paletteID(rgb RGB) (uint8, error) {
	key := rgbKey(rgb)
	if id, ok := db.paletteMap[key]; ok {
		return id, nil
	}
	if len(db.palette) >= 255 {
		return 0, errors.New("timeshadedb: palette limit exceeded")
	}
	db.palette = append(db.palette, rgb)
	id := uint8(len(db.palette))
	db.paletteMap[key] = id
	if db.batchMode {
		return id, nil
	}
	return id, db.writePalette()
}

func (db *DB) writePalette() error {
	var buf bytes.Buffer
	buf.WriteString("TPAL")
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(db.palette)))
	for _, c := range db.palette {
		buf.WriteByte(c.R)
		buf.WriteByte(c.G)
		buf.WriteByte(c.B)
	}
	return os.WriteFile(filepath.Join(db.path, "palette.bin"), buf.Bytes(), 0o644)
}

func (db *DB) readPalette() error {
	data, err := os.ReadFile(filepath.Join(db.path, "palette.bin"))
	if err != nil {
		return err
	}
	if len(data) < 8 || string(data[:4]) != "TPAL" {
		return errors.New("timeshadedb: bad palette file")
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != formatVersion {
		return fmt.Errorf("timeshadedb: unsupported palette version %d", version)
	}
	count := binary.LittleEndian.Uint16(data[6:8])
	if len(data) != 8+int(count)*3 {
		return errors.New("timeshadedb: truncated palette file")
	}
	db.palette = make([]RGB, count)
	for i := range db.palette {
		p := 8 + i*3
		db.palette[i] = RGB{R: data[p], G: data[p+1], B: data[p+2]}
		db.paletteMap[rgbKey(db.palette[i])] = uint8(i + 1)
	}
	return nil
}

func (db *DB) observeIndexSeq(idx tileIndex) {
	db.seqMu.Lock()
	defer db.seqMu.Unlock()
	for _, s := range idx.snapshots {
		if s.EventSeq >= db.nextSeq {
			db.nextSeq = s.EventSeq + 1
		}
	}
	for _, d := range idx.deltas {
		if d.LastEventSeq >= db.nextSeq {
			db.nextSeq = d.LastEventSeq + 1
		}
	}
}

func (db *DB) openTileWAL(t *tileState) error {
	if err := os.MkdirAll(filepath.Join(db.path, "wal"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(walPath(db.path, t.x, t.y), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if info.Size() == 0 {
		if _, err := f.Write([]byte("TWAL")); err != nil {
			_ = f.Close()
			return err
		}
		if err := binary.Write(f, binary.LittleEndian, formatVersion); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
	}
	t.wal = f
	return nil
}

func (db *DB) replayTileWAL(t *tileState) error {
	events, err := readWALEvents(walPath(db.path, t.x, t.y))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ev := range events {
		if int(ev.pos) >= len(t.pixels) {
			return fmt.Errorf("timeshadedb: WAL position out of bounds for tile %d,%d: %d", t.x, t.y, ev.pos)
		}
		if t.pixels[ev.pos] == ev.c {
			continue
		}
		t.pixels[ev.pos] = ev.c
		t.builder = append(t.builder, ev)
		t.changedPlacements++
		t.changesSinceSnap++
		if ev.seq >= db.nextSeq {
			db.nextSeq = ev.seq + 1
		}
	}
	return nil
}

func appendWALEvent(t *tileState, ev deltaEvent) error {
	if t.wal == nil {
		return errors.New("timeshadedb: WAL is not open")
	}
	var buf [17]byte
	binary.LittleEndian.PutUint32(buf[0:4], ev.sec)
	binary.LittleEndian.PutUint32(buf[4:8], ev.pos)
	buf[8] = ev.c
	binary.LittleEndian.PutUint64(buf[9:17], ev.seq)
	if _, err := t.wal.Write(buf[:]); err != nil {
		return err
	}
	return t.wal.Sync()
}

func readWALEvents(path string) ([]deltaEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < 6 || string(data[:4]) != "TWAL" {
		return nil, fmt.Errorf("timeshadedb: bad WAL header: %s", path)
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != formatVersion {
		return nil, fmt.Errorf("timeshadedb: unsupported WAL version %d", version)
	}
	if (len(data)-6)%17 != 0 {
		return nil, fmt.Errorf("timeshadedb: truncated WAL: %s", path)
	}
	events := make([]deltaEvent, 0, (len(data)-6)/17)
	for pos := 6; pos < len(data); pos += 17 {
		events = append(events, deltaEvent{
			sec: binary.LittleEndian.Uint32(data[pos : pos+4]),
			pos: binary.LittleEndian.Uint32(data[pos+4 : pos+8]),
			c:   data[pos+8],
			seq: binary.LittleEndian.Uint64(data[pos+9 : pos+17]),
		})
	}
	return events, nil
}

func (db *DB) clearTileWAL(t *tileState) error {
	var errs []error
	if t.wal != nil {
		if err := t.wal.Close(); err != nil {
			errs = append(errs, err)
		}
		t.wal = nil
	}
	if err := os.Remove(walPath(db.path, t.x, t.y)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func rgbKey(rgb RGB) uint32 {
	return uint32(rgb.R)<<16 | uint32(rgb.G)<<8 | uint32(rgb.B)
}

func tileSlot(x, y int) int {
	return y*TileCols + x
}

func tileDataPath(root string, x, y int) string {
	return filepath.Join(root, "tiles", fmt.Sprintf("%02d_%02d.tdat", x, y))
}

func tileIndexPath(root string, x, y int) string {
	return filepath.Join(root, "tiles", fmt.Sprintf("%02d_%02d.tidx", x, y))
}

func statsPath(root string) string {
	return filepath.Join(root, "stats.json")
}

func walPath(root string, x, y int) string {
	return filepath.Join(root, "wal", fmt.Sprintf("tile_%02d_%02d.active", x, y))
}

func unixSec(t time.Time) uint32 {
	if t.Unix() < 0 {
		return 0
	}
	if t.Unix() > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(t.Unix())
}
