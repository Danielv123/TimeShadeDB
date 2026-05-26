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
	"sort"
	"strconv"
	"strings"
)

const (
	ChunkSize           = 32
	ChunkPixelCount     = ChunkSize * ChunkSize
	chunkEventsDir      = "factorio_chunks"
	chunkEventsFile     = "events.jsonl"
	chunkDatastoresDir  = "datastores"
	chunkPayloadHex     = ChunkPixelCount * 2 * 2
	chunkPayloadBytes   = ChunkPixelCount * 2
	chunkFrameHeaderLen = uint16(37)
)

type chunkDatastore struct {
	key          DatastoreKey
	chunks       map[ChunkCoord]*factorioChunkState
	events       map[ChunkCoord][]chunkLogEntry
	latestTick   uint64
	latestChunk  ChunkCoord
	latestRowSeq uint64
}

type factorioChunkState struct {
	pixels       [ChunkPixelCount]uint16
	seen         bool
	latestTick   uint64
	latestRowSeq uint64
	index        chunkIndex
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
	Key    DatastoreKey
	Tick   uint64
	Chunk  ChunkCoord
	Pixels [ChunkPixelCount]uint16
}

func (db *DB) ingestChunk(ctx context.Context, in ChunkIngest) (*IngestChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db.closed {
		return nil, errors.New("timeshadedb: database is closed")
	}
	if db.readOnly {
		return nil, errors.New("timeshadedb: database opened read-only")
	}
	if len(in.Pixels) != ChunkPixelCount {
		return nil, fmt.Errorf("timeshadedb: chunk has %d pixels, expected %d", len(in.Pixels), ChunkPixelCount)
	}
	if err := validateDatastoreKey(in.Key); err != nil {
		return nil, err
	}

	db.chunkMu.Lock()
	defer db.chunkMu.Unlock()
	if err := db.loadChunksLocked(); err != nil {
		return nil, err
	}
	ds := db.chunkDatastoreLocked(in.Key)
	chunk := ds.chunkLocked(in.Chunk)
	seq := db.nextChunkSeq
	db.nextChunkSeq++
	wasSeen := chunk.seen

	changes := make([]chunkPixelChange, 0, ChunkPixelCount)
	for i, color := range in.Pixels {
		if !chunk.seen || chunk.pixels[i] != color {
			chunk.pixels[i] = color
			changes = append(changes, chunkPixelChange{Pos: uint16(i), Color: color})
		}
	}
	chunk.seen = true
	chunk.latestTick = in.Tick
	chunk.latestRowSeq = seq

	entry := chunkLogEntry{Seq: seq, Tick: in.Tick, Key: in.Key, Chunk: in.Chunk, Changes: changes}
	if err := db.writeChunkFrameLocked(ds, in.Chunk, chunk, entry, !wasSeen); err != nil {
		return nil, err
	}
	if err := db.appendChunkLogEntryLocked(entry); err != nil {
		return nil, err
	}
	ds.events[in.Chunk] = append(ds.events[in.Chunk], entry)
	ds.latestTick = in.Tick
	ds.latestChunk = in.Chunk
	ds.latestRowSeq = seq

	return &IngestChunkResult{
		AcceptedRows:      1,
		ChangedPixels:     uint64(len(changes)),
		UnchangedPixels:   uint64(ChunkPixelCount - len(changes)),
		DatastoresTouched: 1,
		LatestRowSeq:      seq,
	}, nil
}

func (db *DB) chunkAt(ctx context.Context, opts ChunkAtOptions) (*ChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDatastoreKey(opts.Key); err != nil {
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
	ds := db.chunkDatastores[opts.Key]
	if ds == nil {
		return nil, fmt.Errorf("timeshadedb: datastore not found: %s/%s/%s", opts.Key.SavefileUUID, opts.Key.Surface, opts.Key.Force)
	}
	chunk := ds.chunks[opts.Chunk]
	if chunk != nil && chunk.seen && opts.Tick >= chunk.latestTick {
		pixels := make([]uint16, ChunkPixelCount)
		copy(pixels, chunk.pixels[:])
		return &ChunkResult{
			Key:          opts.Key,
			Chunk:        opts.Chunk,
			Tick:         opts.Tick,
			Width:        ChunkSize,
			Height:       ChunkSize,
			Pixels:       pixels,
			SnapshotTick: chunk.latestTick,
		}, nil
	}
	events := ds.events[opts.Chunk]
	if len(events) == 0 {
		return nil, fmt.Errorf("timeshadedb: chunk not found: %d,%d", opts.Chunk.X, opts.Chunk.Y)
	}
	var pixels [ChunkPixelCount]uint16
	var seen bool
	var replayed int
	var snapshotTick uint64
	for _, entry := range events {
		if entry.Tick > opts.Tick {
			break
		}
		seen = true
		snapshotTick = entry.Tick
		for _, change := range entry.Changes {
			if int(change.Pos) >= len(pixels) {
				return nil, fmt.Errorf("timeshadedb: chunk log position out of bounds: %d", change.Pos)
			}
			pixels[change.Pos] = change.Color
			replayed++
		}
	}
	if !seen {
		return nil, fmt.Errorf("timeshadedb: chunk has no data at tick %d: %d,%d", opts.Tick, opts.Chunk.X, opts.Chunk.Y)
	}
	out := make([]uint16, ChunkPixelCount)
	copy(out, pixels[:])
	return &ChunkResult{
		Key:          opts.Key,
		Chunk:        opts.Chunk,
		Tick:         opts.Tick,
		Width:        ChunkSize,
		Height:       ChunkSize,
		Pixels:       out,
		SnapshotTick: snapshotTick,
		Replayed:     replayed,
	}, nil
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

func ParseChunkTSVRow(savefileUUID, line string) (ParsedChunkRow, error) {
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
	pixels, err := decodeRGB565Hex(colorData)
	if err != nil {
		return ParsedChunkRow{}, err
	}
	return ParsedChunkRow{
		Key:    key,
		Tick:   tick,
		Chunk:  ChunkCoord{X: int32(chunkX), Y: int32(chunkY)},
		Pixels: pixels,
	}, nil
}

func ParseChunkTSV(savefileUUID string, body *bufio.Scanner) ([]ParsedChunkRow, error) {
	var rows []ParsedChunkRow
	for lineNo := 1; body.Scan(); lineNo++ {
		line := body.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		row, err := ParseChunkTSVRow(savefileUUID, line)
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
		pixels[i] = uint16(raw[i*2])<<8 | uint16(raw[i*2+1])
	}
	return pixels, nil
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
	loadedBinary, err := db.loadChunkDataFilesLocked()
	if err != nil {
		return err
	}
	if loadedBinary {
		db.chunkLoaded = true
		return nil
	}
	path := chunkLogPath(db.path)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		db.chunkLoaded = true
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		var entry chunkLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("timeshadedb: chunk log line %d: %w", lineNo, err)
		}
		if err := db.applyChunkLogEntryLocked(entry); err != nil {
			return fmt.Errorf("timeshadedb: chunk log line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	db.chunkLoaded = true
	return nil
}

func (db *DB) applyChunkLogEntryLocked(entry chunkLogEntry) error {
	if err := validateDatastoreKey(entry.Key); err != nil {
		return err
	}
	ds := db.chunkDatastoreLocked(entry.Key)
	chunk := ds.chunkLocked(entry.Chunk)
	for _, change := range entry.Changes {
		if int(change.Pos) >= ChunkPixelCount {
			return fmt.Errorf("chunk event position out of bounds: %d", change.Pos)
		}
		chunk.pixels[change.Pos] = change.Color
	}
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
	return nil
}

func (db *DB) writeChunkFrameLocked(ds *chunkDatastore, coord ChunkCoord, chunk *factorioChunkState, entry chunkLogEntry, snapshot bool) error {
	dsDir := chunkDatastorePath(db.path, ds.key)
	if err := os.MkdirAll(filepath.Join(dsDir, "chunks"), 0o755); err != nil {
		return err
	}
	if err := writeChunkMetadata(dsDir, ds.key); err != nil {
		return err
	}
	dataPath := chunkDataPath(db.path, ds.key, coord)
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
		if err := writeChunkDataHeader(f, coord); err != nil {
			return err
		}
	}

	var kind uint8
	var raw []byte
	if snapshot {
		kind = frameKindSnapshot
		raw = encodeChunkSnapshot(chunk.pixels)
	} else {
		kind = frameKindDelta
		raw = encodeChunkDeltaPayload(entry.Tick, entry.Changes)
	}
	offset, compLen, checksum, err := db.writeChunkFrame(f, kind, entry.Tick, uint32(len(entry.Changes)), raw)
	if err != nil {
		return err
	}
	if snapshot {
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
	}
	return writeChunkIndex(db.path, ds.key, coord, chunk.index)
}

func (db *DB) writeChunkFrame(f *os.File, kind uint8, tick uint64, eventCount uint32, raw []byte) (uint64, uint32, uint32, error) {
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, 0, err
	}
	payload, err := db.compressChunkPayload(raw, kind)
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
	if err := f.Sync(); err != nil {
		return 0, 0, 0, err
	}
	return uint64(offset), uint32(len(payload)), checksum, nil
}

func (db *DB) compressChunkPayload(raw []byte, kind uint8) ([]byte, error) {
	if db.compressor != nil {
		return db.compressor.compress(raw, db.compressionLevel(raw, kind))
	}
	return compressZstd(raw, db.compressionLevel(raw, kind))
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

func (db *DB) appendChunkLogEntryLocked(entry chunkLogEntry) error {
	if err := os.MkdirAll(filepath.Join(db.path, chunkEventsDir), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(chunkLogPath(db.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (db *DB) loadChunkDataFilesLocked() (bool, error) {
	root := filepath.Join(db.path, chunkEventsDir, chunkDatastoresDir)
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
		chunksDir := filepath.Join(dsDir, "chunks")
		chunkFiles, err := os.ReadDir(chunksDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, file := range chunkFiles {
			if file.IsDir() || filepath.Ext(file.Name()) != ".cidx" {
				continue
			}
			idxPath := filepath.Join(chunksDir, file.Name())
			coord, idx, err := readChunkIndex(idxPath)
			if err != nil {
				return false, err
			}
			if err := db.loadChunkFramesLocked(ds, coord, idx); err != nil {
				return false, err
			}
			loaded = true
		}
	}
	return loaded, nil
}

func (db *DB) loadChunkFramesLocked(ds *chunkDatastore, coord ChunkCoord, idx chunkIndex) error {
	dataPath := chunkDataPath(db.path, ds.key, coord)
	f, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := readChunkDataHeader(f, coord); err != nil {
		return err
	}
	chunk := ds.chunkLocked(coord)
	chunk.index = idx
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

func writeChunkMetadata(dsDir string, key DatastoreKey) error {
	data, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dsDir, "metadata.json"), data, 0o644)
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

func chunkLogPath(root string) string {
	return filepath.Join(root, chunkEventsDir, chunkEventsFile)
}

func chunkDatastoreID(key DatastoreKey) string {
	sum := sha256.Sum256([]byte(key.SavefileUUID + "\x00" + key.Surface + "\x00" + key.Force))
	return hex.EncodeToString(sum[:16])
}

func chunkDatastorePath(root string, key DatastoreKey) string {
	return filepath.Join(root, chunkEventsDir, chunkDatastoresDir, chunkDatastoreID(key))
}

func chunkFileStem(coord ChunkCoord) string {
	return fmt.Sprintf("cx_%d_cy_%d", coord.X, coord.Y)
}

func chunkDataPath(root string, key DatastoreKey, coord ChunkCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "chunks", chunkFileStem(coord)+".cdat")
}

func chunkIndexPath(root string, key DatastoreKey, coord ChunkCoord) string {
	return filepath.Join(chunkDatastorePath(root, key), "chunks", chunkFileStem(coord)+".cidx")
}
