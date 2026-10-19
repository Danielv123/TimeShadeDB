package terrain

import (
	"encoding/binary"
	"errors"
	"fmt"

	"timeshadedb/internal/v3/core"
)

// errPayload reports a malformed terrain payload (after decompression).
var errPayload = errors.New("terrain: malformed payload")

// lut maps RGB565 to palette index + 1 (0 = not in the palette).
type lut []uint32

func newLUT() lut { return make(lut, 1<<16) }

// chunkChange is one chunk of a delta block: the positions that changed (nil = all
// 1024, in order) and their new palette indices.
type chunkChange struct {
	block uint8
	pos   []uint16
	idx   []uint16
}

// maxIndex returns the largest palette index in the changes.
func maxIndex(cs []chunkChange) uint16 {
	var m uint16
	for i := range cs {
		for _, v := range cs[i].idx {
			if v > m {
				m = v
			}
		}
	}
	return m
}

// diffChunk compares the committed block with the new pixels and returns the change,
// or false when they are equal. Pixels must already be in the palette.
func diffChunk(block uint8, old *core.TileImage, px *core.ChunkPixels, pal lut, scratch []uint16) (chunkChange, bool) {
	pos := scratch[:0]
	for row := 0; row < core.ChunkSize; row++ {
		base := core.TilePixelIndex(block, row*core.ChunkSize)
		o := old.Pix[base : base+core.ChunkSize]
		n := px[row*core.ChunkSize : (row+1)*core.ChunkSize]
		for i := range n {
			if o[i] != n[i] {
				pos = append(pos, uint16(row*core.ChunkSize+i))
			}
		}
	}
	if len(pos) == 0 {
		return chunkChange{}, false
	}
	c := chunkChange{block: block}
	if len(pos) > core.ChunkPixelCount/2 {
		c.idx = make([]uint16, core.ChunkPixelCount)
		for i, v := range px {
			c.idx[i] = uint16(pal[v] - 1)
		}
		return c, true
	}
	c.pos = append([]uint16(nil), pos...)
	c.idx = make([]uint16, len(pos))
	for i, p := range pos {
		c.idx[i] = uint16(pal[px[p]] - 1)
	}
	return c, true
}

// encodeDelta serialises a delta block (FORMAT.md 5.3), changes sorted by block.
func encodeDelta(dst []byte, cs []chunkChange, wide bool) []byte {
	for i := range cs {
		c := &cs[i]
		dst = binary.AppendUvarint(dst, uint64(c.block))
		n := len(c.idx)
		dst = binary.AppendUvarint(dst, uint64(n))
		switch {
		case c.pos == nil:
			// n == 1024: every pixel in order, no positions.
		case n <= 128:
			prev := -1
			for _, p := range c.pos {
				dst = binary.AppendUvarint(dst, uint64(int(p)-prev-1))
				prev = int(p)
			}
		default:
			var bm [128]byte
			for _, p := range c.pos {
				bm[p>>3] |= 1 << (p & 7)
			}
			dst = append(dst, bm[:]...)
		}
		if wide {
			for _, v := range c.idx {
				dst = binary.LittleEndian.AppendUint16(dst, v)
			}
		} else {
			for _, v := range c.idx {
				dst = append(dst, byte(v))
			}
		}
	}
	return dst
}

// deltaVisitor receives one decoded chunk of a delta block. pos is nil when every
// pixel is given; idx holds n raw indices (u8 or u16 LE per wide).
type deltaVisitor func(block uint8, n int, pos []uint16, idx []byte) error

// walkDelta parses a delta block. scratch is reused for positions.
func walkDelta(p []byte, wide bool, scratch []uint16, visit deltaVisitor) error {
	w := 1
	if wide {
		w = 2
	}
	prevBlock := -1
	for len(p) > 0 {
		b, k := binary.Uvarint(p)
		if k <= 0 || b > 255 || int(b) <= prevBlock {
			return fmt.Errorf("%w: block", errPayload)
		}
		prevBlock = int(b)
		p = p[k:]
		n64, k := binary.Uvarint(p)
		if k <= 0 || n64 < 1 || n64 > core.ChunkPixelCount {
			return fmt.Errorf("%w: pixel count", errPayload)
		}
		p = p[k:]
		n := int(n64)
		var pos []uint16
		switch {
		case n == core.ChunkPixelCount:
		case n <= 128:
			pos = scratch[:0]
			prev := -1
			for i := 0; i < n; i++ {
				d, k := binary.Uvarint(p)
				if k <= 0 {
					return fmt.Errorf("%w: position", errPayload)
				}
				p = p[k:]
				if d >= core.ChunkPixelCount {
					return fmt.Errorf("%w: position range", errPayload)
				}
				v := prev + 1 + int(d)
				if v >= core.ChunkPixelCount {
					return fmt.Errorf("%w: position range", errPayload)
				}
				pos = append(pos, uint16(v))
				prev = v
			}
		default:
			if len(p) < 128 {
				return fmt.Errorf("%w: bitmap", errPayload)
			}
			pos = scratch[:0]
			for i := 0; i < 128; i++ {
				for bits := p[i]; bits != 0; bits &= bits - 1 {
					pos = append(pos, uint16(i*8+trailingZeros8(bits)))
				}
			}
			p = p[128:]
			if len(pos) != n {
				return fmt.Errorf("%w: bitmap count", errPayload)
			}
		}
		if len(p) < n*w {
			return fmt.Errorf("%w: indices", errPayload)
		}
		if err := visit(uint8(b), n, pos, p[:n*w]); err != nil {
			return err
		}
		p = p[n*w:]
	}
	return nil
}

func trailingZeros8(b byte) int {
	n := 0
	for b&1 == 0 {
		b >>= 1
		n++
	}
	return n
}

// applyDelta applies a delta block to an RGB565 tile image through the palette.
func applyDelta(im *core.TileImage, p []byte, wide bool, pal []uint16, scratch []uint16) error {
	return walkDelta(p, wide, scratch, func(block uint8, n int, pos []uint16, idx []byte) error {
		get := func(i int) (uint16, error) {
			var v int
			if wide {
				v = int(binary.LittleEndian.Uint16(idx[2*i:]))
			} else {
				v = int(idx[i])
			}
			if v >= len(pal) {
				return 0, fmt.Errorf("%w: palette index %d of %d", errPayload, v, len(pal))
			}
			return pal[v], nil
		}
		if pos == nil {
			for row := 0; row < core.ChunkSize; row++ {
				base := core.TilePixelIndex(block, row*core.ChunkSize)
				for x := 0; x < core.ChunkSize; x++ {
					c, err := get(row*core.ChunkSize + x)
					if err != nil {
						return err
					}
					im.Pix[base+x] = c
				}
			}
			return nil
		}
		for i, ps := range pos {
			c, err := get(i)
			if err != nil {
				return err
			}
			im.Pix[core.TilePixelIndex(block, int(ps))] = c
		}
		return nil
	})
}

// encodeKeyframe serialises the full tile (262,144 indices, row-major).
func encodeKeyframe(dst []byte, im *core.TileImage, pal lut) ([]byte, bool) {
	var m uint32
	for _, c := range im.Pix {
		if v := pal[c]; v > m {
			m = v
		}
	}
	wide := m-1 > 255
	if wide {
		for _, c := range im.Pix {
			dst = binary.LittleEndian.AppendUint16(dst, uint16(pal[c]-1))
		}
		return dst, true
	}
	n := len(dst)
	dst = append(dst, make([]byte, core.TilePixelCount)...)
	out := dst[n:]
	for i, c := range im.Pix {
		out[i] = byte(pal[c] - 1)
	}
	return dst, false
}

// decodeKeyframe fills im from a keyframe payload.
func decodeKeyframe(im *core.TileImage, p []byte, wide bool, pal []uint16) error {
	if wide {
		if len(p) != 2*core.TilePixelCount {
			return fmt.Errorf("%w: keyframe length %d", errPayload, len(p))
		}
		for i := range im.Pix {
			v := int(binary.LittleEndian.Uint16(p[2*i:]))
			if v >= len(pal) {
				return fmt.Errorf("%w: palette index %d", errPayload, v)
			}
			im.Pix[i] = pal[v]
		}
		return nil
	}
	if len(p) != core.TilePixelCount {
		return fmt.Errorf("%w: keyframe length %d", errPayload, len(p))
	}
	if len(pal) < 256 {
		for i, v := range p {
			if int(v) >= len(pal) {
				return fmt.Errorf("%w: palette index %d", errPayload, v)
			}
			im.Pix[i] = pal[v]
		}
		return nil
	}
	pal = pal[:256]
	for i, v := range p {
		im.Pix[i] = pal[v]
	}
	return nil
}
