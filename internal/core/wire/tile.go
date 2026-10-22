// Package wire holds the reference encoders and decoders of the v3 wire formats
// (docs/FORMAT.md, "Wire formats"): TSV3 tiles, TSD3 tile deltas, TSP3 palettes and
// MVW3 mover windows. Bodies are uncompressed; HTTP Content-Encoding (zstd or gzip)
// compresses them in transit. WP3 (tile encoding) and WP4 (movers) may use these
// directly or write faster versions, but their output must decode here and be
// byte-identical for the same input. web/src/api/wire.ts decodes the same formats and
// is tested against web/fixtures, which TestFixtures generates from this package.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"

	"timeshadedb/internal/core"
)

// ErrFormat reports a malformed or unsupported body.
var ErrFormat = errors.New("wire: bad format")

// TileFormat is the pixel encoding of a TSV3 tile or TSD3 delta.
type TileFormat uint8

// Tile formats.
const (
	// FormatPal8: one byte per pixel, an index into the datastore palette.
	FormatPal8 TileFormat = 1
	// FormatPal16: two bytes (LE) per pixel, a palette index.
	FormatPal16 TileFormat = 2
	// FormatRGB565: two bytes (LE) per pixel, the colour itself. Coarse levels use it.
	FormatRGB565 TileFormat = 3
)

func (f TileFormat) pixelBytes() int {
	if f == FormatPal8 {
		return 1
	}
	return 2
}

// Magic numbers.
const (
	MagicTile    = "TSV3"
	MagicDelta   = "TSD3"
	MagicPalette = "TSP3"
	MagicMovers  = "MVW3"
)

// HeaderSize is the size of the TSV3 and TSD3 headers.
const HeaderSize = 32

// TileHeader is the TSV3 header.
type TileHeader struct {
	Format  TileFormat
	Z       int8
	Live    bool
	X, Y    int32
	Version core.Version
	// PaletteVersion is the smallest palette length that decodes the tile (highest index
	// used + 1); 0 for FormatRGB565.
	PaletteVersion uint32
}

// DeltaHeader is the TSD3 header.
type DeltaHeader struct {
	Format         TileFormat
	X, Y           int32
	From, To       core.Version
	PaletteVersion uint32
	Count          int
}

// LUT maps RGB565 colours to palette indices; -1 means absent.
type LUT []int32

// NewLUT builds the colour-to-index table of a palette.
func NewLUT(p core.Palette) LUT {
	l := make(LUT, 65536)
	for i := range l {
		l[i] = -1
	}
	for i, c := range p.Colors {
		if l[c] < 0 {
			l[c] = int32(i)
		}
	}
	return l
}

// indexPixels maps colours to indices, returning them and the highest index.
func indexPixels(px []uint16, lut LUT) ([]uint16, int, error) {
	out := make([]uint16, len(px))
	maxIdx := 0
	for i, c := range px {
		idx := lut[c]
		if idx < 0 {
			return nil, 0, fmt.Errorf("wire: colour %#04x not in palette", c)
		}
		out[i] = uint16(idx)
		maxIdx = max(maxIdx, int(idx))
	}
	return out, maxIdx, nil
}

func putPixels(dst []byte, f TileFormat, vals []uint16) {
	if f == FormatPal8 {
		for i, v := range vals {
			dst[i] = byte(v)
		}
		return
	}
	for i, v := range vals {
		binary.LittleEndian.PutUint16(dst[2*i:], v)
	}
}

func getPixels(src []byte, f TileFormat, n int, pal []uint16) ([]uint16, error) {
	out := make([]uint16, n)
	for i := 0; i < n; i++ {
		var v uint16
		if f == FormatPal8 {
			v = uint16(src[i])
		} else {
			v = binary.LittleEndian.Uint16(src[2*i:])
		}
		if f != FormatRGB565 {
			if int(v) >= len(pal) {
				return nil, fmt.Errorf("%w: palette index %d beyond palette of %d", ErrFormat, v, len(pal))
			}
			v = pal[v]
		}
		out[i] = v
	}
	return out, nil
}

// EncodeTile encodes a TSV3 tile. With a non-nil lut the tile is paletted (pal8 when
// every index is below 256, else pal16) and h.Format and h.PaletteVersion are set from
// the content; with lut nil it is FormatRGB565. Coarse levels (z < 0) must use nil.
func EncodeTile(h TileHeader, im *core.TileImage, lut LUT) ([]byte, error) {
	if len(im.Pix) != core.TilePixelCount {
		return nil, fmt.Errorf("wire: tile image has %d pixels", len(im.Pix))
	}
	vals := im.Pix
	h.Format, h.PaletteVersion = FormatRGB565, 0
	if lut != nil {
		idx, maxIdx, err := indexPixels(im.Pix, lut)
		if err != nil {
			return nil, err
		}
		vals = idx
		h.PaletteVersion = uint32(maxIdx + 1)
		h.Format = FormatPal8
		if maxIdx > 255 {
			h.Format = FormatPal16
		}
	}
	out := make([]byte, HeaderSize+core.TilePixelCount*h.Format.pixelBytes())
	copy(out, MagicTile)
	out[4] = byte(h.Format)
	out[5] = byte(h.Z)
	if h.Live {
		out[6] = 1
	}
	binary.LittleEndian.PutUint32(out[8:], uint32(h.X))
	binary.LittleEndian.PutUint32(out[12:], uint32(h.Y))
	binary.LittleEndian.PutUint32(out[16:], uint32(h.Version))
	binary.LittleEndian.PutUint32(out[20:], h.PaletteVersion)
	binary.LittleEndian.PutUint16(out[24:], core.TileSize)
	binary.LittleEndian.PutUint16(out[26:], core.TileSize)
	putPixels(out[HeaderSize:], h.Format, vals)
	return out, nil
}

// DecodeTileHeader parses a TSV3 header.
func DecodeTileHeader(b []byte) (TileHeader, error) {
	var h TileHeader
	if len(b) < HeaderSize || string(b[:4]) != MagicTile {
		return h, fmt.Errorf("%w: not a TSV3 tile", ErrFormat)
	}
	h.Format = TileFormat(b[4])
	if h.Format < FormatPal8 || h.Format > FormatRGB565 {
		return h, fmt.Errorf("%w: tile format %d", ErrFormat, h.Format)
	}
	h.Z = int8(b[5])
	h.Live = b[6]&1 != 0
	h.X = int32(binary.LittleEndian.Uint32(b[8:]))
	h.Y = int32(binary.LittleEndian.Uint32(b[12:]))
	h.Version = core.Version(binary.LittleEndian.Uint32(b[16:]))
	h.PaletteVersion = binary.LittleEndian.Uint32(b[20:])
	if binary.LittleEndian.Uint16(b[24:]) != core.TileSize || binary.LittleEndian.Uint16(b[26:]) != core.TileSize {
		return h, fmt.Errorf("%w: tile size", ErrFormat)
	}
	return h, nil
}

// DecodeTile decodes a TSV3 tile to RGB565. pal is required for paletted tiles and must
// have at least h.PaletteVersion entries.
func DecodeTile(b []byte, pal *core.Palette) (TileHeader, *core.TileImage, error) {
	h, err := DecodeTileHeader(b)
	if err != nil {
		return h, nil, err
	}
	if len(b) != HeaderSize+core.TilePixelCount*h.Format.pixelBytes() {
		return h, nil, fmt.Errorf("%w: tile body length %d", ErrFormat, len(b))
	}
	var colors []uint16
	if h.Format != FormatRGB565 {
		if pal == nil || uint32(len(pal.Colors)) < h.PaletteVersion {
			return h, nil, fmt.Errorf("%w: tile needs palette version %d", ErrFormat, h.PaletteVersion)
		}
		colors = pal.Colors
	}
	px, err := getPixels(b[HeaderSize:], h.Format, core.TilePixelCount, colors)
	if err != nil {
		return h, nil, err
	}
	return h, &core.TileImage{Pix: px}, nil
}

// EncodeTileDelta encodes a TSD3 delta (z0 only): entries sorted by block. The format
// and palette version are chosen from the content as in EncodeTile.
func EncodeTileDelta(h DeltaHeader, deltas []core.ChunkDelta, lut LUT) ([]byte, error) {
	if len(deltas) > core.TileBlocks {
		return nil, fmt.Errorf("wire: %d delta entries", len(deltas))
	}
	all := make([]uint16, 0, len(deltas)*core.ChunkPixelCount)
	for i := range deltas {
		if i > 0 && deltas[i].Block <= deltas[i-1].Block {
			return nil, fmt.Errorf("wire: delta blocks not strictly increasing")
		}
		all = append(all, deltas[i].Pixels[:]...)
	}
	h.Format, h.PaletteVersion, h.Count = FormatRGB565, 0, len(deltas)
	if lut != nil {
		idx, maxIdx, err := indexPixels(all, lut)
		if err != nil {
			return nil, err
		}
		all = idx
		h.PaletteVersion = uint32(maxIdx + 1)
		h.Format = FormatPal8
		if maxIdx > 255 {
			h.Format = FormatPal16
		}
	}
	entry := 4 + core.ChunkPixelCount*h.Format.pixelBytes()
	out := make([]byte, HeaderSize+entry*len(deltas))
	copy(out, MagicDelta)
	out[4] = byte(h.Format)
	out[5] = 0 // z
	binary.LittleEndian.PutUint16(out[6:], uint16(len(deltas)))
	binary.LittleEndian.PutUint32(out[8:], uint32(h.X))
	binary.LittleEndian.PutUint32(out[12:], uint32(h.Y))
	binary.LittleEndian.PutUint32(out[16:], uint32(h.From))
	binary.LittleEndian.PutUint32(out[20:], uint32(h.To))
	binary.LittleEndian.PutUint32(out[24:], h.PaletteVersion)
	for i, d := range deltas {
		o := HeaderSize + i*entry
		binary.LittleEndian.PutUint16(out[o:], uint16(d.Block))
		putPixels(out[o+4:], h.Format, all[i*core.ChunkPixelCount:(i+1)*core.ChunkPixelCount])
	}
	return out, nil
}

// DecodeTileDelta decodes a TSD3 delta to RGB565 chunks.
func DecodeTileDelta(b []byte, pal *core.Palette) (DeltaHeader, []core.ChunkDelta, error) {
	var h DeltaHeader
	if len(b) < HeaderSize || string(b[:4]) != MagicDelta {
		return h, nil, fmt.Errorf("%w: not a TSD3 delta", ErrFormat)
	}
	h.Format = TileFormat(b[4])
	if h.Format < FormatPal8 || h.Format > FormatRGB565 || b[5] != 0 {
		return h, nil, fmt.Errorf("%w: delta format %d z %d", ErrFormat, h.Format, int8(b[5]))
	}
	h.Count = int(binary.LittleEndian.Uint16(b[6:]))
	h.X = int32(binary.LittleEndian.Uint32(b[8:]))
	h.Y = int32(binary.LittleEndian.Uint32(b[12:]))
	h.From = core.Version(binary.LittleEndian.Uint32(b[16:]))
	h.To = core.Version(binary.LittleEndian.Uint32(b[20:]))
	h.PaletteVersion = binary.LittleEndian.Uint32(b[24:])
	entry := 4 + core.ChunkPixelCount*h.Format.pixelBytes()
	if h.Count > core.TileBlocks || len(b) != HeaderSize+entry*h.Count {
		return h, nil, fmt.Errorf("%w: delta length %d for %d entries", ErrFormat, len(b), h.Count)
	}
	var colors []uint16
	if h.Format != FormatRGB565 {
		if pal == nil || uint32(len(pal.Colors)) < h.PaletteVersion {
			return h, nil, fmt.Errorf("%w: delta needs palette version %d", ErrFormat, h.PaletteVersion)
		}
		colors = pal.Colors
	}
	out := make([]core.ChunkDelta, h.Count)
	for i := range out {
		o := HeaderSize + i*entry
		blk := binary.LittleEndian.Uint16(b[o:])
		if blk >= core.TileBlocks || (i > 0 && blk <= uint16(out[i-1].Block)) {
			return h, nil, fmt.Errorf("%w: delta block %d", ErrFormat, blk)
		}
		px, err := getPixels(b[o+4:], h.Format, core.ChunkPixelCount, colors)
		if err != nil {
			return h, nil, err
		}
		out[i].Block = uint8(blk)
		copy(out[i].Pixels[:], px)
	}
	return h, out, nil
}

// EncodePalette encodes a TSP3 palette: "TSP3", u32 count, count x u16 RGB565 (LE).
func EncodePalette(p core.Palette) []byte {
	out := make([]byte, 8+2*len(p.Colors))
	copy(out, MagicPalette)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(p.Colors)))
	for i, c := range p.Colors {
		binary.LittleEndian.PutUint16(out[8+2*i:], c)
	}
	return out
}

// DecodePalette decodes a TSP3 palette.
func DecodePalette(b []byte) (core.Palette, error) {
	if len(b) < 8 || string(b[:4]) != MagicPalette {
		return core.Palette{}, fmt.Errorf("%w: not a TSP3 palette", ErrFormat)
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if n < 1 || n > 65536 || len(b) != 8+2*n {
		return core.Palette{}, fmt.Errorf("%w: palette of %d entries in %d bytes", ErrFormat, n, len(b))
	}
	p := core.Palette{Colors: make([]uint16, n)}
	for i := range p.Colors {
		p.Colors[i] = binary.LittleEndian.Uint16(b[8+2*i:])
	}
	if p.Colors[0] != 0 {
		return core.Palette{}, fmt.Errorf("%w: palette entry 0 is %#04x, not 0", ErrFormat, p.Colors[0])
	}
	return p, nil
}
