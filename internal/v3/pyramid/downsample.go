package pyramid

import "timeshadedb/internal/v3/core"

// spread moves the green channel of an RGB565 pixel 16 bits up, leaving room above
// each channel to sum four pixels without carries between channels:
// red sums in bits 11..17, blue in 0..6, green in 21..28.
func spread(v uint16) uint32 {
	x := uint32(v)
	return (x & 0xF81F) | (x&0x07E0)<<16
}

// avgRound is the "+2" of rounding half up, in each channel of a spread sum.
const avgRound = 2 | 2<<11 | 2<<21

// downsampleInto writes the 2x2 average of child into the quadrant of out starting at
// pixel (ox, oy). It is bit-identical to core.Average4 / core.Downsample.
func downsampleInto(out, child []uint16, ox, oy int) {
	const half = core.TileSize / 2
	for y := 0; y < half; y++ {
		r0 := child[2*y*core.TileSize : (2*y+1)*core.TileSize]
		r1 := child[(2*y+1)*core.TileSize : (2*y+2)*core.TileSize]
		o := out[(oy+y)*core.TileSize+ox : (oy+y)*core.TileSize+ox+half]
		for x := range o {
			s := spread(r0[2*x]) + spread(r0[2*x+1]) + spread(r1[2*x]) + spread(r1[2*x+1]) + avgRound
			o[x] = uint16((s>>2)&0xF81F | (s>>18)&0x07E0)
		}
	}
}

// downsample builds a parent tile from its four children (core.TileCoord.Children
// order; nil = all uncharted). Same result as core.Downsample, about 1.4x faster
// (BenchmarkDownsample: 0.56 ms against 0.80 ms).
func downsample(children [4]*core.TileImage) *core.TileImage {
	out := core.NewTileImage()
	const half = core.TileSize / 2
	for q, c := range children {
		if c == nil {
			continue
		}
		downsampleInto(out.Pix, c.Pix, (q%2)*half, (q/2)*half)
	}
	return out
}
