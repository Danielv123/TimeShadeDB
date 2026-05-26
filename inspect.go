package timeshadedb

import (
	"errors"
	"fmt"
)

type TileIndexInfo struct {
	Tile            TileCoord        `json:"tile"`
	Width           int              `json:"width"`
	Height          int              `json:"height"`
	DataFileSize    int64            `json:"data_file_size"`
	SnapshotCount   int              `json:"snapshot_count"`
	DeltaFrameCount int              `json:"delta_frame_count"`
	Snapshots       []SnapshotInfo   `json:"snapshots"`
	DeltaFrames     []DeltaFrameInfo `json:"delta_frames"`
}

type SnapshotInfo struct {
	TimestampSec       uint32 `json:"timestamp_sec"`
	FrameOffset        uint64 `json:"frame_offset"`
	FrameHeaderLen     uint16 `json:"frame_header_len"`
	CompressedLen      uint32 `json:"compressed_len"`
	RawLen             uint32 `json:"raw_len"`
	FirstDeltaFrameIdx uint32 `json:"first_delta_frame_idx"`
	EventSeq           uint64 `json:"event_seq"`
	Checksum           uint32 `json:"checksum"`
}

type DeltaFrameInfo struct {
	MinTimestampSec uint32 `json:"min_timestamp_sec"`
	MaxTimestampSec uint32 `json:"max_timestamp_sec"`
	FrameOffset     uint64 `json:"frame_offset"`
	FrameHeaderLen  uint16 `json:"frame_header_len"`
	CompressedLen   uint32 `json:"compressed_len"`
	RawLen          uint32 `json:"raw_len"`
	EventCount      uint32 `json:"event_count"`
	FirstEventSeq   uint64 `json:"first_event_seq"`
	LastEventSeq    uint64 `json:"last_event_seq"`
	Checksum        uint32 `json:"checksum"`
}

func (db *DB) InspectIndex(tile TileCoord) (*TileIndexInfo, error) {
	if db.closed {
		return nil, errors.New("timeshadedb: database is closed")
	}
	if tile.X < 0 || tile.X >= TileCols || tile.Y < 0 || tile.Y >= TileRows {
		return nil, fmt.Errorf("timeshadedb: tile out of bounds: %d,%d", tile.X, tile.Y)
	}
	t := db.tiles[tileSlot(tile.X, tile.Y)]
	info := &TileIndexInfo{
		Tile:            tile,
		Width:           t.w,
		Height:          t.h,
		SnapshotCount:   len(t.index.snapshots),
		DeltaFrameCount: len(t.index.deltas),
		Snapshots:       make([]SnapshotInfo, len(t.index.snapshots)),
		DeltaFrames:     make([]DeltaFrameInfo, len(t.index.deltas)),
	}
	if t.file != nil {
		if stat, err := t.file.Stat(); err == nil {
			info.DataFileSize = stat.Size()
		}
	}
	for i, s := range t.index.snapshots {
		info.Snapshots[i] = SnapshotInfo{
			TimestampSec:       s.TimestampSec,
			FrameOffset:        s.FrameOffset,
			FrameHeaderLen:     s.FrameHeaderLen,
			CompressedLen:      s.CompressedLen,
			RawLen:             s.RawLen,
			FirstDeltaFrameIdx: s.FirstDeltaFrameIdx,
			EventSeq:           s.EventSeq,
			Checksum:           s.Checksum,
		}
	}
	for i, d := range t.index.deltas {
		info.DeltaFrames[i] = DeltaFrameInfo{
			MinTimestampSec: d.MinTimestampSec,
			MaxTimestampSec: d.MaxTimestampSec,
			FrameOffset:     d.FrameOffset,
			FrameHeaderLen:  d.FrameHeaderLen,
			CompressedLen:   d.CompressedLen,
			RawLen:          d.RawLen,
			EventCount:      d.EventCount,
			FirstEventSeq:   d.FirstEventSeq,
			LastEventSeq:    d.LastEventSeq,
			Checksum:        d.Checksum,
		}
	}
	return info, nil
}
