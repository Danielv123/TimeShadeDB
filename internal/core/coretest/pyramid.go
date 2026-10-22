package coretest

import (
	"context"
	"math"
	"testing"

	"timeshadedb/internal/core"
)

// PyramidLatest checks that the latest version of every tile at levels 0..minZ in pyr
// equals the recursive 2x2 average (core.Downsample) of the terrain's latest committed
// z0 content. Run it after populating terrain and letting the pyramid catch up.
func PyramidLatest(t *testing.T, terrain core.TerrainStore, pyr core.TileSource, ds core.DatastoreKey, minZ int8) {
	t.Helper()
	ctx := context.Background()
	const latest = core.TimeMs(math.MaxInt64)
	var expect func(tile core.TileCoord) *core.TileImage
	expect = func(tile core.TileCoord) *core.TileImage {
		if tile.Z == 0 {
			im, _, err := terrain.ReadTile(ctx, ds, tile, latest, false)
			if err != nil {
				return nil
			}
			return im
		}
		var kids [4]*core.TileImage
		found := false
		for i, c := range tile.Children() {
			kids[i] = expect(c)
			found = found || kids[i] != nil
		}
		if !found {
			return nil
		}
		return core.Downsample(kids)
	}
	z0 := get(terrain.ListTiles(ctx, ds, 0))(t)
	for z := int8(0); z >= minZ; z-- {
		want := map[core.TileCoord]bool{}
		for _, ti := range z0 {
			if z == 0 || ti.Version > 0 {
				want[ti.Tile.Ancestor(z)] = true
			}
		}
		got := get(pyr.ListTiles(ctx, ds, z))(t)
		if len(got) != len(want) {
			t.Fatalf("z%d: pyramid lists %d tiles, want %d", z, len(got), len(want))
		}
		for _, ti := range got {
			if !want[ti.Tile] {
				t.Fatalf("z%d: unexpected tile %v", z, ti.Tile)
			}
			st := get(pyr.VersionAt(ctx, ds, ti.Tile, latest))(t)
			if st.Version != ti.Version {
				t.Fatalf("%v: VersionAt(latest) = %d, ListTiles says %d", ti.Tile, st.Version, ti.Version)
			}
			im := get(pyr.ReadVersion(ctx, ds, ti.Tile, ti.Version))(t)
			if exp := expect(ti.Tile); exp == nil || !im.Equal(exp) {
				t.Fatalf("%v: latest content differs from the 2x2 average of z0", ti.Tile)
			}
		}
	}
}
