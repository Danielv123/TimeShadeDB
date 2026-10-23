package overlays

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"

	"timeshadedb/internal/core"
)

const (
	recOpen = 0x01
	recEnd  = 0x02
	// maxRecord bounds one record's payload (a tag text is at most a few KB).
	maxRecord = 1 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

var errRecord = errors.New("bad overlay record")

var kindCodes = map[core.IntervalKind]byte{core.OverlayTag: 1, core.OverlayRecipe: 2, core.OverlayRail: 3}
var codeKinds = [...]core.IntervalKind{1: core.OverlayTag, 2: core.OverlayRecipe, 3: core.OverlayRail}

// appendFrame appends "u32 length | u32 CRC-32C | payload".
func appendFrame(dst, payload []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(payload)))
	dst = binary.LittleEndian.AppendUint32(dst, crc32.Checksum(payload, castagnoli))
	return append(dst, payload...)
}

// scanRecords calls fn for every complete, CRC-valid frame in b until fn returns false,
// and returns the length of the valid prefix.
func scanRecords(b []byte, fn func(payload []byte) bool) int {
	off := 0
	for off+8 <= len(b) {
		n := int(binary.LittleEndian.Uint32(b[off:]))
		if n > maxRecord || off+8+n > len(b) {
			break
		}
		p := b[off+8 : off+8+n]
		if crc32.Checksum(p, castagnoli) != binary.LittleEndian.Uint32(b[off+4:]) {
			break
		}
		if !fn(p) {
			break
		}
		off += 8 + n
	}
	return off
}

// validFrameAfter reports whether b holds a complete, CRC-valid frame with a decodable
// record at any offset after from. It tells damage in the middle of a log (valid
// records follow) from a torn tail (nothing valid follows; zero-filled space included,
// since an empty payload does not decode).
func validFrameAfter(b []byte, from int) bool {
	for p := from + 1; p+8 < len(b); p++ {
		n := int(binary.LittleEndian.Uint32(b[p:]))
		if n == 0 || n > maxRecord || p+8+n > len(b) {
			continue
		}
		payload := b[p+8 : p+8+n]
		if payload[0] != recOpen && payload[0] != recEnd {
			continue
		}
		if crc32.Checksum(payload, castagnoli) != binary.LittleEndian.Uint32(b[p+4:]) {
			continue
		}
		if _, err := decodeRecord(payload); err == nil {
			return true
		}
	}
	return false
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// appendContent appends every field of iv except Start and End.
func appendContent(b []byte, iv *core.Interval) []byte {
	b = append(b, kindCodes[iv.Kind])
	b = appendString(b, iv.Key)
	b = binary.AppendVarint(b, iv.Instance)
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(iv.X))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(iv.Y))
	b = appendString(b, iv.Icon)
	b = appendString(b, iv.Text)
	b = appendString(b, iv.Recipe)
	b = appendString(b, iv.Quality)
	b = appendString(b, iv.Name)
	return append(b, iv.Direction)
}

func encodeOpen(ord uint32, iv *core.Interval) []byte {
	b := []byte{recOpen}
	b = binary.AppendUvarint(b, uint64(ord))
	b = binary.AppendVarint(b, int64(iv.Start))
	return appendContent(b, iv)
}

func encodeEnd(ord uint32, end core.TimeMs) []byte {
	b := []byte{recEnd}
	b = binary.AppendUvarint(b, uint64(ord))
	return binary.AppendVarint(b, int64(end))
}

type record struct {
	end  bool
	ord  uint64
	endT core.TimeMs
	iv   core.Interval
}

type reader struct {
	b   []byte
	off int
	err bool
}

func (r *reader) uvarint() uint64 {
	if r.err {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		r.err = true
		return 0
	}
	r.off += n
	return v
}

func (r *reader) varint() int64 {
	if r.err {
		return 0
	}
	v, n := binary.Varint(r.b[r.off:])
	if n <= 0 {
		r.err = true
		return 0
	}
	r.off += n
	return v
}

func (r *reader) byte() byte {
	if r.err || r.off >= len(r.b) {
		r.err = true
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

func (r *reader) f64() float64 {
	if r.err || r.off+8 > len(r.b) {
		r.err = true
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(r.b[r.off:]))
	r.off += 8
	return v
}

func (r *reader) str() string {
	n := r.uvarint()
	if r.err || n > uint64(len(r.b)-r.off) {
		r.err = true
		return ""
	}
	s := string(r.b[r.off : r.off+int(n)])
	r.off += int(n)
	return s
}

func decodeRecord(p []byte) (record, error) {
	r := &reader{b: p}
	var rec record
	switch r.byte() {
	case recEnd:
		rec.end = true
		rec.ord = r.uvarint()
		rec.endT = core.TimeMs(r.varint())
	case recOpen:
		iv := &rec.iv
		rec.ord = r.uvarint()
		iv.Start = core.TimeMs(r.varint())
		k := r.byte()
		if k == 0 || int(k) >= len(codeKinds) {
			return rec, errRecord
		}
		iv.Kind = codeKinds[k]
		iv.Key = r.str()
		iv.Instance = r.varint()
		iv.X = r.f64()
		iv.Y = r.f64()
		iv.Icon = r.str()
		iv.Text = r.str()
		iv.Recipe = r.str()
		iv.Quality = r.str()
		iv.Name = r.str()
		iv.Direction = r.byte()
	default:
		return rec, errRecord
	}
	if r.err || r.off != len(p) {
		return rec, errRecord
	}
	return rec, nil
}
