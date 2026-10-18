package memstore_test

import (
	"context"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	. "timeshadedb/internal/v3/core/memstore"
)

func TestTerrainContract(t *testing.T) {
	coretest.TerrainStore(t, func(t *testing.T) core.TerrainStore { return NewTerrain() })
}

func TestTerrainDifferentialSelf(t *testing.T) {
	coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore { return NewTerrain() }, coretest.DiffOptions{ExactVersions: true})
}

func TestMoverContract(t *testing.T) {
	coretest.MoverStore(t, func(t *testing.T) core.MoverStore { return NewMovers() })
}

func TestOverlayContract(t *testing.T) {
	coretest.OverlayStore(t, func(t *testing.T) core.OverlayStore { return NewOverlays() })
}

func TestSeriesContract(t *testing.T) {
	coretest.SeriesStore(t, func(t *testing.T) core.SeriesStore { return NewSeriesStore() })
}

func TestPyramid(t *testing.T) {
	ctx := context.Background()
	ter := NewTerrain()
	ds := coretest.DS
	// Two z0 tiles under the same z-1 tile, and one far away.
	ups := []core.ChunkUpdate{
		{DS: ds, Chunk: core.ChunkCoord{X: -8, Y: -8}, T: 1000, Pixels: coretest.Fill(0xF800)},
		{DS: ds, Chunk: core.ChunkCoord{X: 8, Y: -8}, T: 2000, Pixels: coretest.Fill(0x07E0)},
		{DS: ds, Chunk: core.ChunkCoord{X: 100, Y: 100}, T: 61_000, Pixels: coretest.Fill(0x001F)},
	}
	if _, err := ter.Apply(ctx, ups); err != nil {
		t.Fatal(err)
	}
	if err := ter.Commit(ctx, 120_000); err != nil {
		t.Fatal(err)
	}
	p := NewPyramid(ter)
	coretest.PyramidLatest(t, ter, p, ds, -4)

	parent := core.TileCoord{Z: -1, X: 0, Y: 0}
	infos, err := p.ListTiles(ctx, ds, -1)
	if err != nil || len(infos) != 2 || infos[0].Tile != parent || infos[0].Version != 1 || infos[0].FirstT != 60_000 {
		t.Fatalf("z-1 tiles = %+v, %v", infos, err)
	}
	st, err := p.VersionAt(ctx, ds, parent, 59_999)
	if err != nil || st.Version != 0 {
		t.Fatalf("parent before first change = %+v, %v", st, err)
	}
	im, err := p.ReadVersion(ctx, ds, parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Chunk (-8,-8) is the top-left 32x32 of z0 tile (0,0): 16x16 pixels at z-1 (0,0).
	if im.Pix[0] != 0xF800 || im.Pix[15*core.TileSize+15] != 0xF800 || im.Pix[16] != 0 {
		t.Fatalf("z-1 pixels wrong: %x %x %x", im.Pix[0], im.Pix[15*core.TileSize+15], im.Pix[16])
	}
	// Chunk (8,-8) is block 0 of z0 tile (1,0): top-left of the right half.
	if im.Pix[256] != 0x07E0 {
		t.Fatalf("right child pixel = %x", im.Pix[256])
	}
	hist, err := p.History(ctx, ds, parent, 0, 1<<40)
	if err != nil || len(hist) != 1 || hist[0].T != 60_000 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if _, err := p.VersionAt(ctx, ds, core.TileCoord{Z: -1, X: 50, Y: 50}, 1<<40); err == nil {
		t.Fatal("empty coarse tile served")
	}
}
