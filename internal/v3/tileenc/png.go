package tileenc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"sync"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

var pngBuffers = sync.Pool{New: func() any { return new(png.EncoderBuffer) }}

type pngPool struct{}

func (pngPool) Get() *png.EncoderBuffer  { return pngBuffers.Get().(*png.EncoderBuffer) }
func (pngPool) Put(b *png.EncoderBuffer) { pngBuffers.Put(b) }

var pngEncoder = png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: pngPool{}}

// EncodePNG encodes a tile as PNG for external use (PLAN.md §4.5: the viewer uses TSV3).
// Uncharted pixels (0) are transparent. With a non-nil lut and at most 256 palette
// entries in use the image is 8-bit paletted (palette = the datastore palette prefix the
// tile needs, so the output depends only on content and palette order); otherwise it is
// RGBA. Colours expand by bit replication (core.RGB565ToRGB).
func EncodePNG(im *core.TileImage, pal *core.Palette, lut wire.LUT) ([]byte, error) {
	rect := image.Rect(0, 0, core.TileSize, core.TileSize)
	var img image.Image
	paletted := false
	if lut != nil && pal != nil {
		pim := image.NewPaletted(rect, nil)
		maxIdx := int32(0)
		paletted = true
		for i, c := range im.Pix {
			idx := lut[c]
			if idx < 0 {
				return nil, ErrColour
			}
			if idx > 255 {
				paletted = false
				break
			}
			maxIdx = max(maxIdx, idx)
			pim.Pix[i] = uint8(idx)
		}
		if paletted {
			cp := make(color.Palette, maxIdx+1)
			for i := range cp {
				r, g, b := core.RGB565ToRGB(pal.Colors[i])
				a := uint8(255)
				if pal.Colors[i] == 0 {
					r, g, b, a = 0, 0, 0, 0
				}
				cp[i] = color.NRGBA{R: r, G: g, B: b, A: a}
			}
			pim.Palette = cp
			img = pim
		}
	}
	if !paletted {
		rgba := image.NewNRGBA(rect)
		for i, c := range im.Pix {
			if c == 0 {
				continue
			}
			r, g, b := core.RGB565ToRGB(c)
			o := 4 * i
			rgba.Pix[o], rgba.Pix[o+1], rgba.Pix[o+2], rgba.Pix[o+3] = r, g, b, 255
		}
		img = rgba
	}
	var buf bytes.Buffer
	if err := pngEncoder.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
