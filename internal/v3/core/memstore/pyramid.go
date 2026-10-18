package memstore

import (
	"context"
	"fmt"
	"sort"

	"timeshadedb/internal/v3/core"
)

// Pyramid is the reference core.TileSource for every level z <= 0, built on a Terrain.
//
// Oracle rule (schedule-free): a coarse tile gets a new version at every effective time
// at which any of its z0 descendants got one, and its content is the recursive 2x2
// average (core.Downsample) of its children's committed content at that time. The real
// pyramid (WP3) snapshots coarse levels on a schedule, so it matches this oracle on the
// latest state and at the times it snapshots, not version for version.
type Pyramid struct {
	T *Terrain
}

var _ core.TileSource = (*Pyramid)(nil)

// NewPyramid returns a pyramid over t.
func NewPyramid(t *Terrain) *Pyramid { return &Pyramid{T: t} }

// descendants returns the existing z0 tiles under tile (which may itself be z0).
func (p *Pyramid) descendants(d *terrainDS, tile core.TileCoord) []*terrainTile {
	var out []*terrainTile
	for tc, tl := range d.tiles {
		if tc.Ancestor(tile.Z) == tile {
			out = append(out, tl)
		}
	}
	return out
}

// changeTimes returns the sorted distinct effective times of every version of the given
// z0 tiles.
func changeTimes(tiles []*terrainTile) []core.TimeMs {
	seen := map[core.TimeMs]bool{}
	var out []core.TimeMs
	for _, tl := range tiles {
		for _, v := range tl.versions {
			if !seen[v.eff] {
				seen[v.eff] = true
				out = append(out, v.eff)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// coarse resolves a coarse tile and its change times.
func (p *Pyramid) coarse(key core.DatastoreKey, tile core.TileCoord) (*terrainDS, []core.TimeMs, error) {
	if p.T.closed {
		return nil, nil, core.ErrClosed
	}
	if !tile.Valid() {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	d := p.T.ds[key]
	if d == nil {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	times := changeTimes(p.descendants(d, tile))
	if len(times) == 0 {
		return d, nil, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	return d, times, nil
}

// contentAt returns the committed content of tile (any level) at time t, or nil when
// nothing under it holds data.
func (p *Pyramid) contentAt(d *terrainDS, tile core.TileCoord, t core.TimeMs) *core.TileImage {
	if tile.Z == 0 {
		tl := d.tiles[tile]
		if tl == nil {
			return nil
		}
		return tl.content(tl.versionAt(t))
	}
	var kids [4]*core.TileImage
	found := false
	for i, c := range tile.Children() {
		kids[i] = p.contentAt(d, c, t)
		found = found || kids[i] != nil
	}
	if !found {
		return nil
	}
	return core.Downsample(kids)
}

// VersionAt implements core.TileSource.
func (p *Pyramid) VersionAt(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs) (core.TileState, error) {
	if tile.Z == 0 {
		return p.T.VersionAt(ctx, key, tile, t)
	}
	p.T.mu.RLock()
	defer p.T.mu.RUnlock()
	_, times, err := p.coarse(key, tile)
	if err != nil {
		return core.TileState{}, err
	}
	n := sort.Search(len(times), func(i int) bool { return times[i] > t })
	return core.TileState{Version: core.Version(n)}, nil
}

// ReadVersion implements core.TileSource.
func (p *Pyramid) ReadVersion(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, v core.Version) (*core.TileImage, error) {
	if tile.Z == 0 {
		return p.T.ReadVersion(ctx, key, tile, v)
	}
	p.T.mu.RLock()
	defer p.T.mu.RUnlock()
	d, times, err := p.coarse(key, tile)
	if err != nil {
		return nil, err
	}
	if int(v) > len(times) {
		return nil, fmt.Errorf("%w: %v %v v%d", core.ErrNoVersion, key, tile, v)
	}
	if v == 0 {
		return core.NewTileImage(), nil
	}
	im := p.contentAt(d, tile, times[v-1])
	if im == nil {
		im = core.NewTileImage()
	}
	return im, nil
}

// History implements core.TileSource.
func (p *Pyramid) History(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t0, t1 core.TimeMs) ([]core.VersionPoint, error) {
	if tile.Z == 0 {
		return p.T.History(ctx, key, tile, t0, t1)
	}
	p.T.mu.RLock()
	defer p.T.mu.RUnlock()
	_, times, err := p.coarse(key, tile)
	if err != nil {
		return nil, err
	}
	var out []core.VersionPoint
	for i, t := range times {
		if t >= t0 && t < t1 {
			out = append(out, core.VersionPoint{T: t, Version: core.Version(i + 1)})
		}
	}
	return out, nil
}

// ListTiles implements core.TileSource: every tile at level z with a committed
// descendant.
func (p *Pyramid) ListTiles(ctx context.Context, key core.DatastoreKey, z int8) ([]core.TileInfo, error) {
	if z == 0 {
		return p.T.ListTiles(ctx, key, 0)
	}
	if z > 0 || z < core.MinZ {
		return nil, fmt.Errorf("%w: level %d", core.ErrBadTile, z)
	}
	p.T.mu.RLock()
	defer p.T.mu.RUnlock()
	if p.T.closed {
		return nil, core.ErrClosed
	}
	d := p.T.ds[key]
	if d == nil {
		return nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	groups := map[core.TileCoord][]*terrainTile{}
	for tc, tl := range d.tiles {
		if len(tl.versions) > 0 {
			a := tc.Ancestor(z)
			groups[a] = append(groups[a], tl)
		}
	}
	out := make([]core.TileInfo, 0, len(groups))
	for tc, tiles := range groups {
		times := changeTimes(tiles)
		out = append(out, core.TileInfo{Tile: tc, Version: core.Version(len(times)), FirstT: times[0], LastT: times[len(times)-1]})
	}
	sortTileInfos(out)
	return out, nil
}
