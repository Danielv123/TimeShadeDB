package pyramid

import (
	"encoding/binary"
	"errors"
	"fmt"

	"timeshadedb/internal/v3/core"
)

// Record payloads of coarse series (docs/v3/FORMAT.md 5.4). Both are zstd frames:
//
//   - snapshot (kind 3): 512 x 512 u16 RGB565 LE, row-major.
//   - delta (kind 1, wide flag set): the pixels that changed since the previous record,
//     grouped by 32 x 32 block of the coarse tile (block b covers pixels
//     (32*(b&15) + px, 32*(b>>4) + py)), in increasing block order. Per block: uvarint
//     block, uvarint n (1..1024), positions exactly as in the terrain delta block
//     (FORMAT.md 5.3: none when n = 1024, n uvarint gaps when n <= 128, else a 128-byte
//     bitmap), then n u16 LE RGB565 values.

var errPayload = errors.New("pyramid: corrupt record payload")

// appendSnapshot appends the raw (uncompressed) snapshot body of im.
func appendSnapshot(dst []byte, im *core.TileImage) []byte {
	n := len(dst)
	dst = append(dst, make([]byte, 2*core.TilePixelCount)...)
	out := dst[n:]
	for i, v := range im.Pix {
		binary.LittleEndian.PutUint16(out[2*i:], v)
	}
	return dst
}

func decodeSnapshot(raw []byte, im *core.TileImage) error {
	if len(raw) != 2*core.TilePixelCount {
		return fmt.Errorf("%w: snapshot of %d bytes", errPayload, len(raw))
	}
	for i := range im.Pix {
		im.Pix[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return nil
}

// appendDelta appends the raw delta body from prev to next and returns it with the
// number of changed pixels.
func appendDelta(dst []byte, prev, next *core.TileImage) ([]byte, int) {
	var pos [core.ChunkPixelCount]uint16
	var vals [core.ChunkPixelCount]uint16
	total := 0
	for b := 0; b < core.TileBlocks; b++ {
		bx, by := (b%core.TileChunks)*core.ChunkSize, (b/core.TileChunks)*core.ChunkSize
		n := 0
		for py := 0; py < core.ChunkSize; py++ {
			row := (by+py)*core.TileSize + bx
			pr, nr := prev.Pix[row:row+core.ChunkSize], next.Pix[row:row+core.ChunkSize]
			for px := 0; px < core.ChunkSize; px++ {
				if pr[px] != nr[px] {
					pos[n] = uint16(py*core.ChunkSize + px)
					vals[n] = nr[px]
					n++
				}
			}
		}
		if n == 0 {
			continue
		}
		total += n
		dst = binary.AppendUvarint(dst, uint64(b))
		dst = binary.AppendUvarint(dst, uint64(n))
		switch {
		case n == core.ChunkPixelCount:
		case n <= 128:
			prevPos := -1
			for _, p := range pos[:n] {
				dst = binary.AppendUvarint(dst, uint64(int(p)-prevPos-1))
				prevPos = int(p)
			}
		default:
			var bm [core.ChunkPixelCount / 8]byte
			for _, p := range pos[:n] {
				bm[p>>3] |= 1 << (p & 7)
			}
			dst = append(dst, bm[:]...)
		}
		for _, v := range vals[:n] {
			dst = binary.LittleEndian.AppendUint16(dst, v)
		}
	}
	return dst, total
}

// applyDelta applies a raw delta body to im.
func applyDelta(raw []byte, im *core.TileImage) error {
	var pos [core.ChunkPixelCount]uint16
	lastBlock := -1
	for len(raw) > 0 {
		b, k := binary.Uvarint(raw)
		if k <= 0 || b >= core.TileBlocks || int(b) <= lastBlock {
			return fmt.Errorf("%w: delta block", errPayload)
		}
		raw = raw[k:]
		lastBlock = int(b)
		n64, k := binary.Uvarint(raw)
		if k <= 0 || n64 < 1 || n64 > core.ChunkPixelCount {
			return fmt.Errorf("%w: delta count", errPayload)
		}
		raw = raw[k:]
		n := int(n64)
		switch {
		case n == core.ChunkPixelCount:
			for i := range pos {
				pos[i] = uint16(i)
			}
		case n <= 128:
			p := -1
			for i := 0; i < n; i++ {
				gap, k := binary.Uvarint(raw)
				if k <= 0 {
					return fmt.Errorf("%w: delta positions", errPayload)
				}
				raw = raw[k:]
				p += int(gap) + 1
				if p >= core.ChunkPixelCount {
					return fmt.Errorf("%w: delta position %d", errPayload, p)
				}
				pos[i] = uint16(p)
			}
		default:
			if len(raw) < core.ChunkPixelCount/8 {
				return fmt.Errorf("%w: delta bitmap", errPayload)
			}
			c := 0
			for p := 0; p < core.ChunkPixelCount; p++ {
				if raw[p>>3]&(1<<(p&7)) != 0 {
					if c == n {
						return fmt.Errorf("%w: delta bitmap count", errPayload)
					}
					pos[c] = uint16(p)
					c++
				}
			}
			if c != n {
				return fmt.Errorf("%w: delta bitmap count", errPayload)
			}
			raw = raw[core.ChunkPixelCount/8:]
		}
		if len(raw) < 2*n {
			return fmt.Errorf("%w: delta values", errPayload)
		}
		for i := 0; i < n; i++ {
			im.Pix[core.TilePixelIndex(uint8(b), int(pos[i]))] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		raw = raw[2*n:]
	}
	return nil
}
