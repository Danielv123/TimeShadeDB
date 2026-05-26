package timeshadedb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
)

type CompactStats struct {
	Tiles           int     `json:"tiles"`
	FramesRewritten int     `json:"frames_rewritten"`
	BytesBefore     int64   `json:"bytes_before"`
	BytesAfter      int64   `json:"bytes_after"`
	Ratio           float64 `json:"ratio"`
}

type compactFrame struct {
	offset      uint64
	kind        uint8
	snapshotIdx int
	deltaIdx    int
}

func (db *DB) Compact(ctx context.Context) (*CompactStats, error) {
	if db.closed {
		return nil, errors.New("timeshadedb: database is closed")
	}
	if db.readOnly {
		return nil, errors.New("timeshadedb: database opened read-only")
	}
	stats := &CompactStats{}
	for _, t := range db.tiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t == nil {
			continue
		}
		if err := db.flushDelta(t); err != nil {
			return nil, err
		}
		before, after, frames, err := db.compactTile(ctx, t)
		if err != nil {
			return nil, err
		}
		stats.Tiles++
		stats.FramesRewritten += frames
		stats.BytesBefore += before
		stats.BytesAfter += after
	}
	if stats.BytesBefore > 0 {
		stats.Ratio = float64(stats.BytesAfter) / float64(stats.BytesBefore)
	}
	if err := db.writeStats(); err != nil {
		return nil, err
	}
	return stats, nil
}

func (db *DB) compactTile(ctx context.Context, t *tileState) (int64, int64, int, error) {
	if t.file == nil {
		return 0, 0, 0, errors.New("timeshadedb: tile file is not open")
	}
	if err := t.file.Sync(); err != nil {
		return 0, 0, 0, err
	}
	beforeInfo, err := t.file.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	frames := make([]compactFrame, 0, len(t.index.snapshots)+len(t.index.deltas))
	for i, s := range t.index.snapshots {
		frames = append(frames, compactFrame{offset: s.FrameOffset, kind: frameKindSnapshot, snapshotIdx: i})
	}
	for i, d := range t.index.deltas {
		frames = append(frames, compactFrame{offset: d.FrameOffset, kind: frameKindDelta, deltaIdx: i})
	}
	sort.Slice(frames, func(i, j int) bool {
		return frames[i].offset < frames[j].offset
	})

	tmpPath := tileDataPath(db.path, t.x, t.y) + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, 0, err
	}
	cleanupTmp := true
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close()
		}
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := writeTileDataHeader(tmp, t.x, t.y, t.w, t.h); err != nil {
		return 0, 0, 0, err
	}

	newIndex := tileIndex{
		snapshots: make([]snapshotRecord, len(t.index.snapshots)),
		deltas:    make([]deltaRecord, len(t.index.deltas)),
	}
	for _, frame := range frames {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, err
		}
		info, err := readFrame(t.file, frame.offset)
		if err != nil {
			return 0, 0, 0, err
		}
		raw, err := decompressZstd(info.payload, info.rawLen)
		if err != nil {
			return 0, 0, 0, err
		}
		if crc32.ChecksumIEEE(raw) != info.checksum {
			return 0, 0, 0, fmt.Errorf("timeshadedb: checksum mismatch compacting tile %d,%d", t.x, t.y)
		}
		offset, compLen, checksum, err := db.writeFrame(tmp, info.kind, info.timestampSec, info.maxTimeSec, info.eventCount, raw)
		if err != nil {
			return 0, 0, 0, err
		}
		switch frame.kind {
		case frameKindSnapshot:
			old := t.index.snapshots[frame.snapshotIdx]
			old.FrameOffset = offset
			old.FrameHeaderLen = frameHeaderLen
			old.CompressedLen = compLen
			old.RawLen = uint32(len(raw))
			old.Checksum = checksum
			newIndex.snapshots[frame.snapshotIdx] = old
		case frameKindDelta:
			old := t.index.deltas[frame.deltaIdx]
			old.FrameOffset = offset
			old.FrameHeaderLen = frameHeaderLen
			old.CompressedLen = compLen
			old.RawLen = uint32(len(raw))
			old.Checksum = checksum
			newIndex.deltas[frame.deltaIdx] = old
		default:
			return 0, 0, 0, fmt.Errorf("timeshadedb: unknown frame kind %d", frame.kind)
		}
	}
	if err := tmp.Sync(); err != nil {
		return 0, 0, 0, err
	}
	afterInfo, err := tmp.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, 0, 0, err
	}
	tmpClosed = true
	dataPath := tileDataPath(db.path, t.x, t.y)
	backupPath := dataPath + ".bak"
	if err := t.file.Close(); err != nil {
		return 0, 0, 0, err
	}
	t.file = nil
	_ = os.Remove(backupPath)
	if err := os.Rename(dataPath, backupPath); err != nil {
		return 0, 0, 0, err
	}
	if err := os.Rename(tmpPath, dataPath); err != nil {
		_ = os.Rename(backupPath, dataPath)
		return 0, 0, 0, err
	}
	_ = os.Remove(backupPath)
	cleanupTmp = false
	f, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	t.file = f
	t.index = newIndex
	if err := db.writeTileIndex(t); err != nil {
		return 0, 0, 0, err
	}
	return beforeInfo.Size(), afterInfo.Size(), len(frames), nil
}

func writeTileDataHeader(f *os.File, x, y, w, h int) error {
	if _, err := f.Write([]byte("TDAT")); err != nil {
		return err
	}
	for _, v := range []uint16{formatVersion, uint16(x), uint16(y), uint16(w), uint16(h)} {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func removeCompactionTemps(root string) error {
	return filepath.WalkDir(filepath.Join(root, "tiles"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || filepath.Ext(path) != ".compact" {
			return err
		}
		return os.Remove(path)
	})
}
