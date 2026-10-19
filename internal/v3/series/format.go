// Package series is the on-disk append-only record log of TimeShadeDB v3
// (docs/v3/FORMAT.md 5.1): one .blk payload file and one .idx index of fixed 24-byte
// records per series. It implements core.SeriesStore and core.Series.
//
// Durability: Append writes the .blk record (header and payload in one write) but does
// not fsync. Index records are kept in memory and written to .idx by Sync, after the
// .blk file is fsynced, so an .idx record never points at bytes that are not durable.
// On a crash the missing index records are rebuilt from .blk when the series is opened
// again, and torn or corrupt tails are truncated (Recover).
//
// Memory: an open series holds its index in memory (40 bytes per record). The Store
// keeps at most Options.MaxOpen idle series open and closes the least recently used
// ones, so memory and file handles stay bounded however many series exist.
package series

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"timeshadedb/internal/v3/core"
)

// Sizes of the on-disk structures.
const (
	HeaderSize   = 32
	IndexRecSize = 24
	// MaxPayload is the largest payload a record can hold (the index stores a u24 length).
	MaxPayload = 1<<24 - 1
	magic      = "TSB3"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt reports a record whose header or payload checksum does not match.
var ErrCorrupt = errors.New("series: corrupt record")

// crc returns the CRC-32C of b.
func crc(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// header is a decoded .blk record header.
type header struct {
	kind   core.RecordKind
	flags  core.RecordFlags
	tStart core.TimeMs
	tLen   uint32
	plen   uint32
	pcrc   uint32
}

func putHeader(b []byte, h header) {
	copy(b[0:4], magic)
	b[4] = byte(h.kind)
	b[5] = byte(h.flags)
	b[6], b[7] = 0, 0
	binary.LittleEndian.PutUint64(b[8:], uint64(h.tStart))
	binary.LittleEndian.PutUint32(b[16:], h.tLen)
	binary.LittleEndian.PutUint32(b[20:], h.plen)
	binary.LittleEndian.PutUint32(b[24:], h.pcrc)
	binary.LittleEndian.PutUint32(b[28:], crc(b[:28]))
}

// parseHeader decodes and checks a header. It does not check the payload.
func parseHeader(b []byte) (header, error) {
	if len(b) < HeaderSize || string(b[0:4]) != magic {
		return header{}, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(b[28:]) != crc(b[:28]) {
		return header{}, fmt.Errorf("%w: header checksum", ErrCorrupt)
	}
	h := header{
		kind:   core.RecordKind(b[4]),
		flags:  core.RecordFlags(b[5]),
		tStart: core.TimeMs(binary.LittleEndian.Uint64(b[8:])),
		tLen:   binary.LittleEndian.Uint32(b[16:]),
		plen:   binary.LittleEndian.Uint32(b[20:]),
		pcrc:   binary.LittleEndian.Uint32(b[24:]),
	}
	if !validKind(h.kind) || h.plen > MaxPayload {
		return header{}, fmt.Errorf("%w: bad kind or length", ErrCorrupt)
	}
	return h, nil
}

func validKind(k core.RecordKind) bool {
	return k == core.RecordDelta || k == core.RecordKeyframe || k == core.RecordSnapshot
}

// entry is one in-memory index record.
type entry struct {
	tStart core.TimeMs
	off    int64 // offset of the record header in .blk
	tLen   uint32
	plen   uint32
	// deltas is the number of RecordDelta records with Seq <= this one. At z0 it is the
	// tile version reached after the record (core.Version).
	deltas uint32
	kind   core.RecordKind
	flags  core.RecordFlags
}

func (e *entry) tEnd() core.TimeMs { return e.tStart + core.TimeMs(e.tLen) }
func (e *entry) end() int64        { return e.off + HeaderSize + int64(e.plen) }

func (e *entry) meta(seq uint64) core.RecordMeta {
	return core.RecordMeta{Seq: seq, TStart: e.tStart, TLen: e.tLen, Kind: e.kind, Flags: e.flags, Len: int(e.plen)}
}

func putIndex(b []byte, e *entry) {
	binary.LittleEndian.PutUint64(b[0:], uint64(e.tStart))
	binary.LittleEndian.PutUint32(b[8:], e.tLen)
	off := uint64(e.off)
	for i := 0; i < 6; i++ {
		b[12+i] = byte(off >> (8 * i))
	}
	b[18] = byte(e.plen)
	b[19] = byte(e.plen >> 8)
	b[20] = byte(e.plen >> 16)
	b[21] = byte(e.kind)
	b[22] = byte(e.flags)
	b[23] = 0
}

func parseIndex(b []byte) entry {
	var off uint64
	for i := 0; i < 6; i++ {
		off |= uint64(b[12+i]) << (8 * i)
	}
	return entry{
		tStart: core.TimeMs(binary.LittleEndian.Uint64(b[0:])),
		tLen:   binary.LittleEndian.Uint32(b[8:]),
		off:    int64(off),
		plen:   uint32(b[18]) | uint32(b[19])<<8 | uint32(b[20])<<16,
		kind:   core.RecordKind(b[21]),
		flags:  core.RecordFlags(b[22]),
	}
}

// matches reports whether an index entry agrees with the header it points at.
func (e *entry) matches(h header) bool {
	return e.kind == h.kind && e.flags == h.flags && e.tStart == h.tStart && e.tLen == h.tLen && e.plen == h.plen
}
