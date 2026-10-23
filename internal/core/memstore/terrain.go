// Package memstore holds in-memory reference implementations of the core store
// interfaces. They keep everything in RAM and are written to be obviously correct,
// not fast: they are the test oracles for the real stores (terrain, pyramid, movers
// and overlays) and fixtures for API tests. Do not use them in
// production.
package memstore

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"timeshadedb/internal/core"
)

// noHorizon marks a datastore or surface that has never been committed.
const noHorizon = core.TimeMs(math.MinInt64)

// Terrain is the reference core.TerrainStore.
type Terrain struct {
	mu     sync.RWMutex
	closed bool
	ds     map[core.DatastoreKey]*terrainDS
}

var _ core.TerrainStore = (*Terrain)(nil)

type terrainDS struct {
	key     core.DatastoreKey
	window  core.TimeMs
	horizon core.TimeMs
	first   core.TimeMs
	last    core.TimeMs
	hasData bool
	palette []uint16
	palIdx  map[uint16]int
	tiles   map[core.TileCoord]*terrainTile
	applies uint64 // arrival counter for tie-breaking
}

type openUpdate struct {
	t       core.TimeMs
	arrival uint64
	pixels  core.ChunkPixels
}

type committedVersion struct {
	eff    core.TimeMs
	deltas []core.ChunkDelta // blocks changed by this version, content after it
}

type terrainTile struct {
	versions []committedVersion
	current  *core.TileImage // content at the latest version
	open     map[core.TimeMs]map[uint8]*openUpdate
	liveSeq  uint64
}

// NewTerrain returns an empty store.
func NewTerrain() *Terrain {
	return &Terrain{ds: map[core.DatastoreKey]*terrainDS{}}
}

func (s *Terrain) ensure(key core.DatastoreKey, w core.TimeMs) *terrainDS {
	d := s.ds[key]
	if d == nil {
		if w <= 0 {
			w = core.DefaultTerrainWindow
		}
		d = &terrainDS{
			key:     key,
			window:  w,
			horizon: noHorizon,
			palette: []uint16{0},
			palIdx:  map[uint16]int{0: 0},
			tiles:   map[core.TileCoord]*terrainTile{},
		}
		s.ds[key] = d
	}
	return d
}

// EnsureDatastore implements core.TerrainStore.
func (s *Terrain) EnsureDatastore(ctx context.Context, key core.DatastoreKey, w core.TimeMs) error {
	if err := key.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return core.ErrClosed
	}
	s.ensure(key, w)
	return nil
}

// Apply implements core.TerrainStore.
func (s *Terrain) Apply(ctx context.Context, updates []core.ChunkUpdate) (core.ApplyResult, error) {
	var res core.ApplyResult
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return res, core.ErrClosed
	}
	for i := range updates {
		u := &updates[i]
		if u.DS.Validate() != nil || (!u.Deleted && u.Pixels == nil) {
			res.Invalid++
			continue
		}
		tile, block := u.Chunk.Tile()
		if !tile.Valid() {
			res.Invalid++
			continue
		}
		d := s.ensure(u.DS, 0)
		if u.T < d.horizon {
			res.Late++
			continue
		}
		var px core.ChunkPixels
		if !u.Deleted {
			px = *u.Pixels
		}
		for _, c := range px {
			if _, ok := d.palIdx[c]; !ok {
				d.palIdx[c] = len(d.palette)
				d.palette = append(d.palette, c)
			}
		}
		tl := d.tiles[tile]
		if tl == nil {
			tl = &terrainTile{current: core.NewTileImage(), open: map[core.TimeMs]map[uint8]*openUpdate{}}
			d.tiles[tile] = tl
		}
		ws := core.WindowStart(u.T, d.window)
		win := tl.open[ws]
		if win == nil {
			win = map[uint8]*openUpdate{}
			tl.open[ws] = win
		}
		d.applies++
		if cur := win[block]; cur == nil || u.T >= cur.t {
			win[block] = &openUpdate{t: u.T, arrival: d.applies, pixels: px}
		}
		tl.liveSeq++
		if !d.hasData || u.T < d.first {
			d.first = u.T
		}
		if !d.hasData || u.T > d.last {
			d.last = u.T
		}
		d.hasData = true
		res.Accepted++
	}
	return res, nil
}

// Commit implements core.TerrainStore.
func (s *Terrain) Commit(ctx context.Context, upTo core.TimeMs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return core.ErrClosed
	}
	for _, d := range s.ds {
		newH := core.WindowStart(upTo, d.window)
		if newH <= d.horizon {
			continue
		}
		for _, tl := range d.tiles {
			var starts []core.TimeMs
			for ws := range tl.open {
				if ws < newH {
					starts = append(starts, ws)
				}
			}
			sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
			for _, ws := range starts {
				win := tl.open[ws]
				delete(tl.open, ws)
				blocks := make([]int, 0, len(win))
				for b := range win {
					blocks = append(blocks, int(b))
				}
				sort.Ints(blocks)
				var deltas []core.ChunkDelta
				for _, b := range blocks {
					u := win[uint8(b)]
					if tl.current.Block(uint8(b)) != u.pixels {
						deltas = append(deltas, core.ChunkDelta{Block: uint8(b), Pixels: u.pixels})
					}
				}
				if len(deltas) == 0 {
					continue
				}
				for i := range deltas {
					tl.current.SetBlock(deltas[i].Block, &deltas[i].Pixels)
				}
				tl.versions = append(tl.versions, committedVersion{eff: ws + d.window, deltas: deltas})
			}
		}
		d.horizon = newH
	}
	return nil
}

// lookup resolves a z0 tile. It never creates state.
func (s *Terrain) lookup(key core.DatastoreKey, tile core.TileCoord) (*terrainDS, *terrainTile, error) {
	if s.closed {
		return nil, nil, core.ErrClosed
	}
	if tile.Z != 0 || !tile.Valid() {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	d := s.ds[key]
	if d == nil {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	tl := d.tiles[tile]
	if tl == nil {
		return d, nil, fmt.Errorf("%w: %v %v", core.ErrNoTile, key, tile)
	}
	return d, tl, nil
}

func (tl *terrainTile) versionAt(t core.TimeMs) core.Version {
	return core.Version(sort.Search(len(tl.versions), func(i int) bool { return tl.versions[i].eff > t }))
}

// liveAt reports whether a live read at t would apply open-window updates. Live
// content is offered only at or after the datastore's newest update time (the open
// windows keep only the newest update per chunk, so earlier instants are unknown).
func liveAt(d *terrainDS, tl *terrainTile, t core.TimeMs) bool {
	return len(tl.open) > 0 && t >= d.horizon && t >= d.last
}

func (tl *terrainTile) state(d *terrainDS, t core.TimeMs) core.TileState {
	st := core.TileState{Version: tl.versionAt(t)}
	if liveAt(d, tl, t) {
		st.Live = true
		st.LiveSeq = tl.liveSeq
	}
	return st
}

// content replays versions 1..v from an empty tile.
func (tl *terrainTile) content(v core.Version) *core.TileImage {
	if int(v) == len(tl.versions) {
		return tl.current.Clone()
	}
	im := core.NewTileImage()
	for i := 0; i < int(v); i++ {
		for j := range tl.versions[i].deltas {
			dl := &tl.versions[i].deltas[j]
			im.SetBlock(dl.Block, &dl.Pixels)
		}
	}
	return im
}

// VersionAt implements core.TileSource.
func (s *Terrain) VersionAt(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs) (core.TileState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return core.TileState{}, err
	}
	return tl.state(d, t), nil
}

// ReadVersion implements core.TileSource.
func (s *Terrain) ReadVersion(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, v core.Version) (*core.TileImage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if int(v) > len(tl.versions) {
		return nil, fmt.Errorf("%w: %v %v v%d", core.ErrNoVersion, key, tile, v)
	}
	return tl.content(v), nil
}

// ReadTile implements core.TerrainStore.
func (s *Terrain) ReadTile(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, core.TileState{}, err
	}
	st := tl.state(d, t)
	im := tl.content(st.Version)
	if live && st.Live {
		// Newest open update per block across all open windows.
		newest := map[uint8]*openUpdate{}
		for _, win := range tl.open {
			for b, u := range win {
				if cur := newest[b]; cur == nil || u.t > cur.t || (u.t == cur.t && u.arrival > cur.arrival) {
					newest[b] = u
				}
			}
		}
		for b, u := range newest {
			im.SetBlock(b, &u.pixels)
		}
	} else {
		st.Live, st.LiveSeq = false, 0
	}
	return im, st, nil
}

// History implements core.TileSource.
func (s *Terrain) History(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t0, t1 core.TimeMs) ([]core.VersionPoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	var out []core.VersionPoint
	for i, v := range tl.versions {
		if v.eff >= t0 && v.eff < t1 {
			out = append(out, core.VersionPoint{T: v.eff, Version: core.Version(i + 1)})
		}
	}
	return out, nil
}

// ListTiles implements core.TileSource. Only z = 0 is served.
func (s *Terrain) ListTiles(ctx context.Context, key core.DatastoreKey, z int8) ([]core.TileInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, core.ErrClosed
	}
	if z != 0 {
		return nil, fmt.Errorf("%w: terrain serves z0 only, got z%d", core.ErrBadTile, z)
	}
	d := s.ds[key]
	if d == nil {
		return nil, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	return d.tileInfos(), nil
}

func (d *terrainDS) tileInfos() []core.TileInfo {
	out := make([]core.TileInfo, 0, len(d.tiles))
	for tc, tl := range d.tiles {
		info := core.TileInfo{Tile: tc, Version: core.Version(len(tl.versions)), Live: len(tl.open) > 0}
		if n := len(tl.versions); n > 0 {
			info.FirstT = tl.versions[0].eff
			info.LastT = tl.versions[n-1].eff
		}
		out = append(out, info)
	}
	sortTileInfos(out)
	return out
}

func sortTileInfos(out []core.TileInfo) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Tile, out[j].Tile
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
}

// Datastores implements core.TerrainStore.
func (s *Terrain) Datastores(ctx context.Context) ([]core.DatastoreInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, core.ErrClosed
	}
	out := make([]core.DatastoreInfo, 0, len(s.ds))
	for _, d := range s.ds {
		info := core.DatastoreInfo{
			Key:            d.key,
			ID:             d.key.ID(),
			Window:         d.window,
			PaletteVersion: uint32(len(d.palette)),
			Tiles:          len(d.tiles),
		}
		if d.horizon != noHorizon {
			info.Horizon = d.horizon
		}
		if d.hasData {
			info.FirstT, info.LastT = d.first, d.last
		}
		first := true
		for tc := range d.tiles {
			if first {
				info.Bounds = [4]int32{tc.X, tc.Y, tc.X, tc.Y}
				first = false
				continue
			}
			info.Bounds[0] = min(info.Bounds[0], tc.X)
			info.Bounds[1] = min(info.Bounds[1], tc.Y)
			info.Bounds[2] = max(info.Bounds[2], tc.X)
			info.Bounds[3] = max(info.Bounds[3], tc.Y)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Palette implements core.TerrainStore.
func (s *Terrain) Palette(ctx context.Context, key core.DatastoreKey) (core.Palette, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return core.Palette{}, core.ErrClosed
	}
	d := s.ds[key]
	if d == nil {
		return core.Palette{}, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	return core.Palette{Colors: append([]uint16(nil), d.palette...)}, nil
}

// ChangedChunks implements core.TerrainStore.
func (s *Terrain) ChangedChunks(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, from, to core.Version) ([]core.ChunkDelta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if from >= to || int(to) > len(tl.versions) {
		return nil, fmt.Errorf("%w: %v %v delta %d-%d", core.ErrNoVersion, key, tile, from, to)
	}
	a, b := tl.content(from), tl.content(to)
	touched := map[uint8]bool{}
	for i := int(from); i < int(to); i++ {
		for _, dl := range tl.versions[i].deltas {
			touched[dl.Block] = true
		}
	}
	var out []core.ChunkDelta
	for blk := 0; blk < core.TileBlocks; blk++ {
		if !touched[uint8(blk)] {
			continue
		}
		nb := b.Block(uint8(blk))
		if a.Block(uint8(blk)) != nb {
			out = append(out, core.ChunkDelta{Block: uint8(blk), Pixels: nb})
		}
	}
	return out, nil
}

// Close implements core.TerrainStore. Open windows are dropped.
func (s *Terrain) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
