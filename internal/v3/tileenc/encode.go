// Package tileenc encodes v3 tiles for the wire and caches the encoded responses
// (docs/v3/FORMAT.md section 7, docs/v3/API.md "Compression"; PLAN.md §4.5, WP3).
//
// The encoders here produce bodies byte-identical to the reference codecs in
// internal/v3/core/wire, but avoid their intermediate buffers. Renderer ties a
// core.TileSource (the pyramid) and a terrain store to a byte-bounded LRU of compressed
// bodies keyed by (resource, version, encoding), so immutable tiles are encoded and
// compressed once.
package tileenc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

// ErrColour reports a pixel colour that is not in the palette used to encode it. The
// Renderer refreshes its palette once when it sees it.
var ErrColour = errors.New("tileenc: colour not in palette")

// TileBodySize returns the TSV3 body size for a format.
func TileBodySize(f wire.TileFormat) int {
	if f == wire.FormatPal8 {
		return wire.HeaderSize + core.TilePixelCount
	}
	return wire.HeaderSize + 2*core.TilePixelCount
}

func putTileHeader(out []byte, h wire.TileHeader) {
	copy(out, wire.MagicTile)
	out[4] = byte(h.Format)
	out[5] = byte(h.Z)
	out[6] = 0
	if h.Live {
		out[6] = 1
	}
	out[7] = 0
	binary.LittleEndian.PutUint32(out[8:], uint32(h.X))
	binary.LittleEndian.PutUint32(out[12:], uint32(h.Y))
	binary.LittleEndian.PutUint32(out[16:], uint32(h.Version))
	binary.LittleEndian.PutUint32(out[20:], h.PaletteVersion)
	binary.LittleEndian.PutUint16(out[24:], core.TileSize)
	binary.LittleEndian.PutUint16(out[26:], core.TileSize)
	binary.LittleEndian.PutUint32(out[28:], 0)
}

// EncodeTile encodes a TSV3 tile, byte-identical to wire.EncodeTile. With a non-nil lut
// the tile is paletted (pal8 when every index is below 256, else pal16) and h.Format and
// h.PaletteVersion are taken from the content; with lut nil it is FormatRGB565 (coarse
// levels). A colour missing from lut returns ErrColour.
func EncodeTile(h wire.TileHeader, im *core.TileImage, lut wire.LUT) ([]byte, error) {
	if len(im.Pix) != core.TilePixelCount {
		return nil, fmt.Errorf("tileenc: tile image has %d pixels", len(im.Pix))
	}
	if lut == nil {
		h.Format, h.PaletteVersion = wire.FormatRGB565, 0
		out := make([]byte, TileBodySize(wire.FormatRGB565))
		putTileHeader(out, h)
		putU16s(out[wire.HeaderSize:], im.Pix)
		return out, nil
	}
	if len(lut) != 65536 {
		return nil, fmt.Errorf("tileenc: LUT has %d entries", len(lut))
	}
	// Optimistic pal8 pass; most palettes have fewer than 256 colours.
	out := make([]byte, TileBodySize(wire.FormatPal8))
	px := out[wire.HeaderSize:]
	var maxIdx int32
	for i, c := range im.Pix {
		idx := lut[c]
		if idx < 0 {
			return nil, fmt.Errorf("%w: %#04x", ErrColour, c)
		}
		if idx > 255 {
			return encodePal16(h, im, lut)
		}
		maxIdx = max(maxIdx, idx)
		px[i] = byte(idx)
	}
	h.Format, h.PaletteVersion = wire.FormatPal8, uint32(maxIdx+1)
	putTileHeader(out, h)
	return out, nil
}

func encodePal16(h wire.TileHeader, im *core.TileImage, lut wire.LUT) ([]byte, error) {
	out := make([]byte, TileBodySize(wire.FormatPal16))
	px := out[wire.HeaderSize:]
	var maxIdx int32
	for i, c := range im.Pix {
		idx := lut[c]
		if idx < 0 {
			return nil, fmt.Errorf("%w: %#04x", ErrColour, c)
		}
		maxIdx = max(maxIdx, idx)
		binary.LittleEndian.PutUint16(px[2*i:], uint16(idx))
	}
	h.Format, h.PaletteVersion = wire.FormatPal16, uint32(maxIdx+1)
	putTileHeader(out, h)
	return out, nil
}

func putU16s(dst []byte, vals []uint16) {
	if nativeLittleEndian {
		copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(vals))), 2*len(vals)))
		return
	}
	for i, v := range vals {
		binary.LittleEndian.PutUint16(dst[2*i:], v)
	}
}

// nativeLittleEndian reports whether u16 slices are already in wire byte order.
var nativeLittleEndian = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// EncodeTileDelta encodes a TSD3 delta (z0), byte-identical to wire.EncodeTileDelta.
// A colour missing from lut returns ErrColour.
func EncodeTileDelta(h wire.DeltaHeader, deltas []core.ChunkDelta, lut wire.LUT) ([]byte, error) {
	if lut != nil {
		for i := range deltas {
			for _, c := range deltas[i].Pixels {
				if lut[c] < 0 {
					return nil, fmt.Errorf("%w: %#04x", ErrColour, c)
				}
			}
		}
	}
	return wire.EncodeTileDelta(h, deltas, lut)
}

// EncodePalette encodes a TSP3 palette holding the first n entries of p.
func EncodePalette(p core.Palette, n int) ([]byte, error) {
	if n < 1 || n > len(p.Colors) {
		return nil, fmt.Errorf("%w: palette version %d of %d", core.ErrNoVersion, n, len(p.Colors))
	}
	return wire.EncodePalette(core.Palette{Colors: p.Colors[:n]}), nil
}
