package terrain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/series"
)

// rawPool holds decompression buffers (a keyframe is up to 512 KiB).
var rawPool = sync.Pool{New: func() any { b := make([]byte, 0, 2*core.TilePixelCount); return &b }}

// lookup resolves a z0 tile without creating anything. It returns the bare sentinel
// errors, so rejecting unknown or invalid tiles does not allocate.
func (s *Store) lookup(key core.DatastoreKey, tile core.TileCoord) (*dsState, *tileState, error) {
	if tile.Z != 0 || !tile.Valid() {
		return nil, nil, core.ErrBadTile
	}
	s.mu.RLock()
	d, closed := s.ds[key], s.closed
	s.mu.RUnlock()
	if closed {
		return nil, nil, core.ErrClosed
	}
	if d == nil {
		return nil, nil, core.ErrUnknownDatastore
	}
	d.mu.RLock()
	tl := d.tiles[tile]
	d.mu.RUnlock()
	if tl == nil {
		return d, nil, core.ErrNoTile
	}
	return d, tl, nil
}

func (s *Store) lookupDS(key core.DatastoreKey) (*dsState, error) {
	s.mu.RLock()
	d, closed := s.ds[key], s.closed
	s.mu.RUnlock()
	if closed {
		return nil, core.ErrClosed
	}
	if d == nil {
		return nil, core.ErrUnknownDatastore
	}
	return d, nil
}

// tileSnap is a consistent copy of a tile's published summary.
type tileSnap struct {
	versions      uint32
	firstT, lastT core.TimeMs
	hasOpen       bool
	liveSeq       uint64
}

func (tl *tileState) snap() tileSnap {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tileSnap{versions: tl.versions, firstT: tl.firstT, lastT: tl.lastT, hasOpen: len(tl.open) > 0 || tl.spilled > 0, liveSeq: tl.liveSeq}
}

// state computes the tile state at t.
func (s *Store) state(d *dsState, tl *tileState, t core.TimeMs) (core.TileState, error) {
	st, _, err := s.stateSnap(d, tl, t)
	return st, err
}

// stateSnap is state that also returns the tile summary it used.
func (s *Store) stateSnap(d *dsState, tl *tileState, t core.TimeMs) (core.TileState, tileSnap, error) {
	d.mu.RLock()
	horizon, last := d.horizon, d.last
	d.mu.RUnlock()
	sn := tl.snap()
	var st core.TileState
	switch {
	case sn.versions == 0 || t < sn.firstT:
		st.Version = 0
	case t >= sn.lastT:
		st.Version = core.Version(sn.versions)
	default:
		h, err := s.series.OpenSeries(tl.name, false)
		if err != nil {
			return st, sn, err
		}
		v := h.VersionAt(t)
		h.Close()
		st.Version = core.Version(min(v, sn.versions))
	}
	if sn.hasOpen && t >= horizon && t >= last {
		st.Live = true
		st.LiveSeq = sn.liveSeq
	}
	return st, sn, nil
}

// image returns the (immutable, shared) content of version v. h may be nil.
func (s *Store) image(d *dsState, tl *tileState, h *series.Series, v core.Version) (*core.TileImage, error) {
	if v == 0 {
		return core.NewTileImage(), nil
	}
	key := cacheKey{d, tl.coord, v}
	if im := s.cache.get(key); im != nil {
		return im, nil
	}
	if h == nil {
		var err error
		if h, err = s.series.OpenSeries(tl.name, false); err != nil {
			return nil, err
		}
		defer h.Close()
	}
	have := func(pv uint32) bool { return s.cache.has(cacheKey{d, tl.coord, core.Version(pv)}) }
	plan, ok := h.PlanVersion(uint32(v), have, 8)
	if !ok {
		return nil, fmt.Errorf("%w: %s %v v%d", core.ErrNoVersion, d.id, tl.coord, v)
	}
	var im *core.TileImage
	if !plan.FromBase {
		if prev := s.cache.get(cacheKey{d, tl.coord, core.Version(plan.Resume)}); prev != nil {
			im = prev.Clone()
		} else {
			plan, _ = h.PlanVersion(uint32(v), nil, 0)
		}
	}
	if im == nil {
		im = core.NewTileImage()
	}
	recs, err := h.ReadRange(context.Background(), plan.First, plan.Last)
	if err != nil {
		return nil, err
	}
	pal := d.pal.snapshot()
	bp := rawPool.Get().(*[]byte)
	defer rawPool.Put(bp)
	var scratch [core.ChunkPixelCount]uint16
	for i := range recs {
		r := &recs[i]
		raw, err := s.dec.DecodeAll(r.Payload, (*bp)[:0])
		if err != nil {
			return nil, fmt.Errorf("terrain: %s record %d: %w", tl.name, r.Seq, err)
		}
		*bp = raw
		wide := r.Flags&core.FlagWide != 0
		switch r.Kind {
		case core.RecordKeyframe, core.RecordSnapshot:
			err = decodeKeyframe(im, raw, wide, pal)
		case core.RecordDelta:
			err = applyDelta(im, raw, wide, pal, scratch[:])
		}
		if err != nil {
			return nil, fmt.Errorf("terrain: %s record %d: %w", tl.name, r.Seq, err)
		}
	}
	s.cache.put(key, im)
	return im, nil
}

// VersionAt implements core.TileSource.
func (s *Store) VersionAt(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs) (core.TileState, error) {
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return core.TileState{}, err
	}
	return s.state(d, tl, t)
}

// ReadVersion implements core.TileSource.
func (s *Store) ReadVersion(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, v core.Version) (*core.TileImage, error) {
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if uint32(v) > tl.snap().versions {
		return nil, fmt.Errorf("%w: %s %v v%d", core.ErrNoVersion, d.id, tile, v)
	}
	im, err := s.image(d, tl, nil, v)
	if err != nil {
		return nil, err
	}
	return im.Clone(), nil
}

// ReadTile implements core.TerrainStore.
func (s *Store) ReadTile(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error) {
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, core.TileState{}, err
	}
	for {
		st, sn, err := s.stateSnap(d, tl, t)
		if err != nil {
			return nil, st, err
		}
		if !live || !st.Live {
			base, err := s.image(d, tl, nil, st.Version)
			if err != nil {
				return nil, st, err
			}
			st.Live, st.LiveSeq = false, 0
			return base.Clone(), st, nil
		}
		// Live at t means t is at or after every accepted update, so the content is the
		// newest committed version plus the open windows. (st.Version can be older only
		// while a commit is publishing tiles before it raises the horizon.)
		base, err := s.image(d, tl, nil, core.Version(sn.versions))
		if err != nil {
			return nil, st, err
		}
		im := base.Clone()
		// Spilled windows of the tile (spill.go) are read first; their chunks count as
		// older arrivals than any in memory.
		spilled, gen, err := s.liveSpills(d, tl)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, st, err
			}
			continue // a commit closed a spilled window and deleted its file: retry
		}
		tl.mu.Lock()
		if tl.versions != sn.versions || tl.spillGen != gen {
			// A commit published a new version (and dropped the windows it closed)
			// after the snapshot: the overlay would miss those windows. Retry; a tile
			// is committed at most once per commit. Likewise a spill that moved
			// windows out of memory after the spilled ones were read.
			tl.mu.Unlock()
			continue
		}
		var newest [core.TileBlocks]*openChunk
		for b, oc := range spilled {
			if oc != nil {
				newest[b] = oc
			}
		}
		for _, win := range tl.open {
			for b, oc := range win.chunks {
				if cur := newest[b]; cur == nil || oc.t > cur.t || (oc.t == cur.t && oc.arrival > cur.arrival) {
					newest[b] = oc
				}
			}
		}
		for b, oc := range newest {
			if oc != nil {
				im.SetBlock(uint8(b), &oc.px)
			}
		}
		st.LiveSeq = tl.liveSeq // the sequence of the content returned
		tl.mu.Unlock()
		return im, st, nil
	}
}

// History implements core.TileSource.
func (s *Store) History(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, t0, t1 core.TimeMs) ([]core.VersionPoint, error) {
	_, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	sn := tl.snap()
	if sn.versions == 0 || t1 <= sn.firstT || t0 > sn.lastT {
		return nil, nil
	}
	h, err := s.series.OpenSeries(tl.name, false)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return h.History(t0, t1, sn.versions), nil
}

// ListTiles implements core.TileSource. Only z = 0 is served.
func (s *Store) ListTiles(ctx context.Context, key core.DatastoreKey, z int8) ([]core.TileInfo, error) {
	d, err := s.lookupDS(key)
	if err != nil && err != core.ErrUnknownDatastore {
		return nil, err
	}
	if z != 0 {
		return nil, fmt.Errorf("%w: terrain serves z0 only, got z%d", core.ErrBadTile, z)
	}
	if err != nil {
		return nil, err
	}
	d.mu.RLock()
	out := make([]core.TileInfo, 0, len(d.tiles))
	for tc, tl := range d.tiles {
		sn := tl.snap()
		info := core.TileInfo{Tile: tc, Version: core.Version(sn.versions), Live: sn.hasOpen}
		if sn.versions > 0 {
			info.FirstT, info.LastT = sn.firstT, sn.lastT
		}
		out = append(out, info)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Tile, out[j].Tile
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	return out, nil
}

// Datastores implements core.TerrainStore.
func (s *Store) Datastores(ctx context.Context) ([]core.DatastoreInfo, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, core.ErrClosed
	}
	dss := make([]*dsState, 0, len(s.ds))
	for _, d := range s.ds {
		dss = append(dss, d)
	}
	s.mu.RUnlock()
	out := make([]core.DatastoreInfo, 0, len(dss))
	for _, d := range dss {
		info := core.DatastoreInfo{
			Key:            d.key,
			ID:             d.id,
			Window:         d.window,
			PaletteVersion: uint32(len(d.pal.snapshot())),
		}
		d.mu.RLock()
		if d.horizon != noHorizon {
			info.Horizon = d.horizon
		}
		if d.hasData {
			info.FirstT, info.LastT = d.first, d.last
		}
		info.Tiles = len(d.tiles)
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
		d.mu.RUnlock()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Palette implements core.TerrainStore.
func (s *Store) Palette(ctx context.Context, key core.DatastoreKey) (core.Palette, error) {
	d, err := s.lookupDS(key)
	if err != nil {
		return core.Palette{}, err
	}
	return core.Palette{Colors: append([]uint16(nil), d.pal.snapshot()...)}, nil
}

// ChangedChunks implements core.TerrainStore.
func (s *Store) ChangedChunks(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, from, to core.Version) ([]core.ChunkDelta, error) {
	d, tl, err := s.lookup(key, tile)
	if err != nil {
		return nil, err
	}
	if from >= to || uint32(to) > tl.snap().versions {
		return nil, fmt.Errorf("%w: %s %v delta %d-%d", core.ErrNoVersion, d.id, tile, from, to)
	}
	h, err := s.series.OpenSeries(tl.name, false)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	first, ok1 := h.VersionSeq(uint32(from) + 1)
	last, ok2 := h.VersionSeq(uint32(to))
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%w: %s %v delta %d-%d", core.ErrNoVersion, d.id, tile, from, to)
	}
	recs, err := h.ReadRange(ctx, first, last)
	if err != nil {
		return nil, err
	}
	var touched [core.TileBlocks]bool
	var scratch [core.ChunkPixelCount]uint16
	bp := rawPool.Get().(*[]byte)
	defer rawPool.Put(bp)
	for i := range recs {
		if recs[i].Kind != core.RecordDelta {
			continue
		}
		raw, err := s.dec.DecodeAll(recs[i].Payload, (*bp)[:0])
		if err != nil {
			return nil, err
		}
		*bp = raw
		err = walkDelta(raw, recs[i].Flags&core.FlagWide != 0, scratch[:], func(b uint8, _ int, _ []uint16, _ []byte) error {
			touched[b] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	a, err := s.image(d, tl, h, from)
	if err != nil {
		return nil, err
	}
	b, err := s.image(d, tl, h, to)
	if err != nil {
		return nil, err
	}
	var out []core.ChunkDelta
	for blk := 0; blk < core.TileBlocks; blk++ {
		if !touched[blk] {
			continue
		}
		nb := b.Block(uint8(blk))
		if a.Block(uint8(blk)) != nb {
			out = append(out, core.ChunkDelta{Block: uint8(blk), Pixels: nb})
		}
	}
	return out, nil
}
