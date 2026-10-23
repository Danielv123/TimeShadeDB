package core

// Colours are RGB565 as Factorio's LuaForce.get_chunk_chart returns them:
// r = v>>11 & 31, g = v>>5 & 63, b = v & 31. 0 means black, which the chart uses for
// uncharted pixels; v3 treats 0 as "uncharted" everywhere.

// ChunkPixels is one chunk's chart, 32 x 32 RGB565, row-major (pixel (px, py) at
// index py*32+px is world tile (32*cx+px, 32*cy+py)).
type ChunkPixels [ChunkPixelCount]uint16

// TileImage is one tile at any level: 512 x 512 RGB565, row-major. At z0 pixel (px, py)
// is world tile (x0+px, y0+py) where (x0, y0) = TileCoord.Origin(). At z < 0 a pixel
// is the recursive 2x2 average (Average4) of the level above.
type TileImage struct {
	Pix []uint16 // len TilePixelCount
}

// NewTileImage returns an all-uncharted tile image.
func NewTileImage() *TileImage {
	return &TileImage{Pix: make([]uint16, TilePixelCount)}
}

// Clone returns a deep copy.
func (im *TileImage) Clone() *TileImage {
	c := NewTileImage()
	copy(c.Pix, im.Pix)
	return c
}

// Equal reports whether both images hold the same pixels.
func (im *TileImage) Equal(o *TileImage) bool {
	if len(im.Pix) != len(o.Pix) {
		return false
	}
	for i := range im.Pix {
		if im.Pix[i] != o.Pix[i] {
			return false
		}
	}
	return true
}

// Block copies block b (a chunk of a z0 tile) out of the image.
func (im *TileImage) Block(block uint8) ChunkPixels {
	var out ChunkPixels
	for row := 0; row < ChunkSize; row++ {
		i := TilePixelIndex(block, row*ChunkSize)
		copy(out[row*ChunkSize:(row+1)*ChunkSize], im.Pix[i:i+ChunkSize])
	}
	return out
}

// SetBlock writes a chunk into block b of the image.
func (im *TileImage) SetBlock(block uint8, px *ChunkPixels) {
	for row := 0; row < ChunkSize; row++ {
		i := TilePixelIndex(block, row*ChunkSize)
		copy(im.Pix[i:i+ChunkSize], px[row*ChunkSize:(row+1)*ChunkSize])
	}
}

// IsEmpty reports whether every pixel is 0 (uncharted).
func (im *TileImage) IsEmpty() bool {
	for _, v := range im.Pix {
		if v != 0 {
			return false
		}
	}
	return true
}

// RGB565 packs 8-bit channels into RGB565 by truncation.
func RGB565(r, g, b uint8) uint16 {
	return uint16(r>>3)<<11 | uint16(g>>2)<<5 | uint16(b>>3)
}

// RGB565ToRGB expands RGB565 to 8-bit channels by bit replication (the same expansion
// the viewer shaders use).
func RGB565ToRGB(v uint16) (r, g, b uint8) {
	r5 := uint8(v >> 11 & 31)
	g6 := uint8(v >> 5 & 63)
	b5 := uint8(v & 31)
	return r5<<3 | r5>>2, g6<<2 | g6>>4, b5<<3 | b5>>2
}

// Average4 is the per-channel average of four RGB565 pixels, rounded half up. It is
// bit-identical to illisis shared/levels.ts average(), so coarse levels can be checked
// against illisis's z1-4 data.
func Average4(a, b, c, d uint16) uint16 {
	ch := func(shift, mask uint16) uint16 {
		sum := (a>>shift)&mask + (b>>shift)&mask + (c>>shift)&mask + (d>>shift)&mask
		return ((sum + 2) >> 2) << shift
	}
	return ch(11, 31) | ch(5, 63) | ch(0, 31)
}

// Downsample builds a parent tile image from its four children (in TileCoord.Children
// order). A nil child counts as all-uncharted.
func Downsample(children [4]*TileImage) *TileImage {
	out := NewTileImage()
	const half = TileSize / 2
	for q, child := range children {
		if child == nil {
			continue
		}
		ox := (q % 2) * half
		oy := (q / 2) * half
		for y := 0; y < half; y++ {
			for x := 0; x < half; x++ {
				i := 2*y*TileSize + 2*x
				out.Pix[(oy+y)*TileSize+ox+x] = Average4(child.Pix[i], child.Pix[i+1], child.Pix[i+TileSize], child.Pix[i+TileSize+1])
			}
		}
	}
	return out
}
