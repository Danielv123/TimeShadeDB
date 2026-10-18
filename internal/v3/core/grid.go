package core

import (
	"fmt"
	"math"
)

// Grid constants (docs/v3/FORMAT.md "Grid"). They are fixed for format version 1.
const (
	// ChunkSize is a Factorio chunk's side in tiles, and a chunk image's side in pixels.
	ChunkSize = 32
	// ChunkPixelCount is the number of pixels in a chunk image.
	ChunkPixelCount = ChunkSize * ChunkSize
	// TileChunks is a z0 tile's side in chunks.
	TileChunks = 16
	// TileBlocks is the number of chunk blocks in a z0 tile (block = chunk, row-major).
	TileBlocks = TileChunks * TileChunks
	// TileSize is every tile's side in pixels, at every level.
	TileSize = TileChunks * ChunkSize
	// TilePixelCount is the number of pixels in a tile image.
	TilePixelCount = TileSize * TileSize
	// GridOriginTiles is the world tile coordinate (x and y) where tile (0, 0) starts at
	// every level. -256 aligns z0 tiles with 512-tile gridworld instances, whose
	// instance (0, 0) covers [-256, 256). Same as illisis keys.ts.
	GridOriginTiles = -256
	// GridOriginChunks is GridOriginTiles in chunks.
	GridOriginChunks = GridOriginTiles / ChunkSize
	// MinZ is the coarsest level. At z = -10 one pixel is 1024 tiles and one tile covers
	// 524,288 tiles, more than a Factorio surface (2,000,000 tiles across is the hard map
	// limit, so -12 would be exhaustive; -10 is enough for every charted area seen).
	MinZ = -10
	// MaxCoord bounds tile x and y at z0 (|x|, |y| <= MaxCoord). The Factorio map limit is
	// +-1,000,000 tiles, about +-1954 z0 tiles. Requests outside are rejected without
	// allocating anything (PLAN.md §4.3 G2).
	MaxCoord = 2048
)

// ChunkCoord is a Factorio chunk position (chunk (cx, cy) covers tiles
// [32*cx, 32*cx+32) x [32*cy, 32*cy+32)).
type ChunkCoord struct {
	X int32 `json:"cx"`
	Y int32 `json:"cy"`
}

// TileCoord is a tile at level Z <= 0. At level z one pixel covers 2^-z x 2^-z world
// tiles, and tile (x, y) covers world tiles
// [GridOriginTiles + x*512*2^-z, GridOriginTiles + (x+1)*512*2^-z) on each axis.
// A z0 tile holds 16 x 16 chunks.
type TileCoord struct {
	Z int8  `json:"z"`
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

func (c ChunkCoord) String() string { return fmt.Sprintf("%d,%d", c.X, c.Y) }
func (t TileCoord) String() string  { return fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y) }

// Scale returns the number of world tiles per pixel side at level z (2^-z).
func Scale(z int8) int64 { return int64(1) << uint(-z) }

// TileSpan returns the number of world tiles a tile at level z covers per side.
func TileSpan(z int8) int64 { return TileSize * Scale(z) }

// Valid reports whether the tile is inside the addressable grid: MinZ <= z <= 0 and
// x, y within the bound for that level.
func (t TileCoord) Valid() bool {
	if t.Z > 0 || t.Z < MinZ {
		return false
	}
	lim := int32(MaxCoord >> uint(-t.Z))
	if lim < 1 {
		lim = 1
	}
	return t.X >= -lim && t.X <= lim && t.Y >= -lim && t.Y <= lim
}

// Tile returns the z0 tile holding chunk c, and c's block index in it (0..255,
// row-major: block = bx + 16*by).
func (c ChunkCoord) Tile() (TileCoord, uint8) {
	ox := int64(c.X) - GridOriginChunks
	oy := int64(c.Y) - GridOriginChunks
	tx := FloorDiv(ox, TileChunks)
	ty := FloorDiv(oy, TileChunks)
	bx := ox - tx*TileChunks
	by := oy - ty*TileChunks
	return TileCoord{Z: 0, X: int32(tx), Y: int32(ty)}, uint8(bx + by*TileChunks)
}

// Chunk returns the chunk at block b of z0 tile t. It is the inverse of ChunkCoord.Tile.
func (t TileCoord) Chunk(block uint8) ChunkCoord {
	bx := int32(block) % TileChunks
	by := int32(block) / TileChunks
	return ChunkCoord{
		X: t.X*TileChunks + GridOriginChunks + bx,
		Y: t.Y*TileChunks + GridOriginChunks + by,
	}
}

// Origin returns the world tile coordinate of the tile's top-left corner.
func (t TileCoord) Origin() (x, y int64) {
	span := TileSpan(t.Z)
	return GridOriginTiles + int64(t.X)*span, GridOriginTiles + int64(t.Y)*span
}

// Contains reports whether world position (x, y) (in tiles) lies in the tile.
func (t TileCoord) Contains(x, y float64) bool {
	return TileAt(x, y, t.Z) == t
}

// TileAt returns the tile at level z holding world position (x, y) in tiles.
func TileAt(x, y float64, z int8) TileCoord {
	span := float64(TileSpan(z))
	return TileCoord{
		Z: z,
		X: int32(math.Floor((x - GridOriginTiles) / span)),
		Y: int32(math.Floor((y - GridOriginTiles) / span)),
	}
}

// Parent returns the tile one level coarser (z-1) that contains t.
func (t TileCoord) Parent() TileCoord {
	return TileCoord{Z: t.Z - 1, X: int32(FloorDiv(int64(t.X), 2)), Y: int32(FloorDiv(int64(t.Y), 2))}
}

// Children returns the four tiles one level finer (z+1) inside t, in the order
// top-left, top-right, bottom-left, bottom-right. It panics at z = 0.
func (t TileCoord) Children() [4]TileCoord {
	if t.Z >= 0 {
		panic("core: z0 tiles have no children")
	}
	z := t.Z + 1
	x, y := t.X*2, t.Y*2
	return [4]TileCoord{{z, x, y}, {z, x + 1, y}, {z, x, y + 1}, {z, x + 1, y + 1}}
}

// Ancestor returns the tile at level z (z <= t.Z) containing t.
func (t TileCoord) Ancestor(z int8) TileCoord {
	for t.Z > z {
		t = t.Parent()
	}
	return t
}

// TilePixelIndex returns the index into a tile image of pixel pos (0..1023, row-major)
// of block b of a z0 tile.
func TilePixelIndex(block uint8, pos int) int {
	bx := int(block) % TileChunks
	by := int(block) / TileChunks
	return (by*ChunkSize+pos/ChunkSize)*TileSize + bx*ChunkSize + pos%ChunkSize
}
