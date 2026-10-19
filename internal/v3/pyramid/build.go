package pyramid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"timeshadedb/internal/v3/core"
)

// BuildStats counts what a build did.
type BuildStats struct {
	// Passes is the number of build passes (one per datastore and build time).
	Passes int `json:"passes"`
	// Built counts tiles whose content was computed; Versions those that got a new
	// version (Snapshots + Deltas); Unchanged those whose average did not change.
	Built     int `json:"built"`
	Versions  int `json:"versions"`
	Snapshots int `json:"snapshots"`
	Deltas    int `json:"deltas"`
	Unchanged int `json:"unchanged"`
	// Bytes is the payload bytes appended (excluding series headers and index);
	// SnapshotBytes the part of it in snapshots.
	Bytes         int64 `json:"bytes"`
	SnapshotBytes int64 `json:"snapshot_bytes"`
}

func (s *BuildStats) add(o BuildStats) {
	s.Passes += o.Passes
	s.Built += o.Built
	s.Versions += o.Versions
	s.Snapshots += o.Snapshots
	s.Deltas += o.Deltas
	s.Unchanged += o.Unchanged
	s.Bytes += o.Bytes
	s.SnapshotBytes += o.SnapshotBytes
}

// Update builds the coarse levels that are due, for every datastore, at T = the
// datastore's horizon (live mode). Call it after each terrain Commit; it is cheap when
// nothing changed (O(z0 tiles) bookkeeping, no pixels read). Coarse answers are final
// only below BuiltUpTo, which Update advances to the horizon it read.
func (p *Pyramid) Update(ctx context.Context) (BuildStats, error) {
	return p.update(ctx, false)
}

// Flush builds every coarse tile with pending z0 changes at the datastore horizon,
// ignoring the schedule, so the latest versions equal the average of the latest
// committed z0 content (coretest.PyramidLatest). Use it on shutdown, after imports and
// in tests.
func (p *Pyramid) Flush(ctx context.Context) (BuildStats, error) {
	return p.update(ctx, true)
}

func (p *Pyramid) update(ctx context.Context, force bool) (BuildStats, error) {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	var total BuildStats
	infos, err := p.terrain.Datastores(ctx)
	if err != nil {
		return total, err
	}
	for _, info := range infos {
		if info.Tiles == 0 || info.Horizon == 0 {
			continue
		}
		d, err := p.ensureDS(ctx, info.Key)
		if err != nil {
			return total, err
		}
		tiles, err := p.terrain.ListTiles(ctx, info.Key, 0)
		if err != nil {
			return total, err
		}
		for _, ti := range tiles {
			if ti.Version > 0 {
				p.propagate(d, ti.Tile, ti.LastT)
			}
		}
		T, prev := info.Horizon, d.prevT
		if prev == math.MinInt64 || prev >= T {
			prev = T - 1 // first pass (or no progress): due only at an exact multiple
		}
		st, err := p.pass(ctx, d, T, prev, force)
		d.mu.Lock()
		d.prevT = max(d.prevT, T)
		d.mu.Unlock()
		total.add(st)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Rebuild builds the coarse history of one datastore from its whole z0 history, as if
// Update had run after every commit, then flushes at the horizon. It is for offline
// builds (after an import, before the database is served) and requires that the
// datastore has no coarse records yet.
func (p *Pyramid) Rebuild(ctx context.Context, key core.DatastoreKey) (BuildStats, error) {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	var total BuildStats
	infos, err := p.terrain.Datastores(ctx)
	if err != nil {
		return total, err
	}
	var info *core.DatastoreInfo
	for i := range infos {
		if infos[i].Key == key {
			info = &infos[i]
		}
	}
	if info == nil {
		return total, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
	}
	d, err := p.ensureDS(ctx, key)
	if err != nil {
		return total, err
	}
	for _, ct := range d.tiles {
		if _, n, _, _ := ct.state(); n > 0 {
			return total, fmt.Errorf("%w: %v", ErrAlreadyBuilt, key)
		}
	}
	if info.Tiles == 0 || info.Horizon == 0 {
		return total, nil
	}
	horizon := info.Horizon

	type z0hist struct {
		tile  core.TileCoord
		times []core.TimeMs
		i     int
	}
	tiles, err := p.terrain.ListTiles(ctx, key, 0)
	if err != nil {
		return total, err
	}
	var hists []*z0hist
	passSet := map[core.TimeMs]struct{}{}
	for _, ti := range tiles {
		if ti.Version == 0 {
			continue
		}
		pts, err := p.terrain.History(ctx, key, ti.Tile, math.MinInt64, horizon+1)
		if err != nil {
			return total, err
		}
		h := &z0hist{tile: ti.Tile, times: make([]core.TimeMs, len(pts))}
		for i, pt := range pts {
			h.times[i] = pt.T
			// Replay what Update after every window's commit would do: a dirty tile
			// at level k is built at the first multiple of its interval at or after
			// the change. Passes at those instants (below the horizon; the final
			// flush covers the rest) give the same versions.
			for k := 1; k <= p.levels(); k++ {
				iv := p.interval(k)
				if k > 1 && iv == p.interval(k-1) {
					continue
				}
				bt := core.TimeMs(-core.FloorDiv(-int64(pt.T), int64(iv)) * int64(iv))
				passSet[bt] = struct{}{}
			}
		}
		hists = append(hists, h)
	}
	passes := make([]core.TimeMs, 0, len(passSet))
	for t := range passSet {
		passes = append(passes, t)
	}
	slices.Sort(passes)
	advance := func(T core.TimeMs) {
		for _, h := range hists {
			moved := false
			for h.i < len(h.times) && h.times[h.i] <= T {
				h.i++
				moved = true
			}
			if moved {
				p.propagate(d, h.tile, h.times[h.i-1])
			}
		}
	}
	for _, T := range passes {
		if T >= horizon {
			break
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		advance(T)
		st, err := p.pass(ctx, d, T, T-1, false)
		total.add(st)
		if err != nil {
			return total, err
		}
	}
	advance(horizon)
	st, err := p.pass(ctx, d, horizon, horizon-1, true)
	d.mu.Lock()
	d.prevT = max(d.prevT, horizon)
	d.mu.Unlock()
	total.add(st)
	return total, err
}

// propagate records that z0 tile has versions up to time lastT and marks every coarse
// ancestor whose content may have changed as dirty.
func (p *Pyramid) propagate(d *dsState, tile core.TileCoord, lastT core.TimeMs) {
	if seen, ok := d.z0seen[tile]; ok && seen >= lastT {
		return
	}
	d.z0seen[tile] = lastT
	for z := int8(-1); z >= p.opt.MinZ; z-- {
		a := tile.Ancestor(z)
		d.mu.RLock()
		ct := d.tiles[a]
		d.mu.RUnlock()
		if ct == nil {
			ct = &ctile{coord: a, descT: math.MinInt64, cleanT: math.MinInt64, firstT: 0, lastT: math.MinInt64}
			d.mu.Lock()
			d.tiles[a] = ct
			d.mu.Unlock()
		}
		if lastT > ct.descT {
			ct.descT = lastT
		}
		if _, n, _, last := ct.state(); n == 0 || ct.descT > max(last, ct.cleanT) {
			d.dirty[a] = struct{}{}
		}
	}
}

// pass builds, at time T, every dirty tile of every level that is due: level k = -z is
// due when a multiple of its interval lies in (prev, T], or always with force. Since
// each interval is a multiple of the finer one, levels are due from k = 1 up to some k.
func (p *Pyramid) pass(ctx context.Context, d *dsState, T, prev core.TimeMs, force bool) (BuildStats, error) {
	if len(d.dirty) == 0 {
		return BuildStats{}, nil
	}
	st := BuildStats{Passes: 1}
	for k := 1; k <= p.levels(); k++ {
		iv := int64(p.interval(k))
		if !force && core.FloorDiv(int64(T), iv) <= core.FloorDiv(int64(prev), iv) {
			break
		}
		z := int8(-k)
		var todo []core.TileCoord
		for c := range d.dirty {
			if c.Z == z {
				todo = append(todo, c)
			}
		}
		slices.SortFunc(todo, func(a, b core.TileCoord) int {
			if a.Y != b.Y {
				return int(a.Y) - int(b.Y)
			}
			return int(a.X) - int(b.X)
		})
		for _, c := range todo {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			d.mu.RLock()
			ct := d.tiles[c]
			d.mu.RUnlock()
			if err := p.buildTile(ctx, d, ct, T, &st); err != nil {
				return st, fmt.Errorf("pyramid: build %v %v at %v: %w", d.key, c, T, err)
			}
		}
	}
	return st, nil
}

// z0At returns the committed z0 content of tile at time T, or nil if it holds nothing.
func (p *Pyramid) z0At(ctx context.Context, key core.DatastoreKey, tile core.TileCoord, T core.TimeMs) (*core.TileImage, error) {
	s, err := p.terrain.VersionAt(ctx, key, tile, T)
	if errors.Is(err, core.ErrNoTile) {
		return nil, nil
	}
	if err != nil || s.Version == 0 {
		return nil, err
	}
	return p.terrain.ReadVersion(ctx, key, tile, s.Version)
}

// buildTile computes a coarse tile's content at T from its children and appends a new
// version if it changed.
func (p *Pyramid) buildTile(ctx context.Context, d *dsState, ct *ctile, T core.TimeMs, st *BuildStats) error {
	if _, n, _, lastT := ct.state(); n > 0 && T <= lastT {
		// Already built for T (the z0 content at T is final), or T went backwards:
		// nothing new can be said about this instant. The tile stays dirty if z0 holds
		// newer versions (an early-flushed window) and is built at a later T.
		return nil
	}
	var kids [4]*core.TileImage
	for i, c := range ct.coord.Children() {
		var err error
		if c.Z == 0 {
			kids[i], err = p.z0At(ctx, d.key, c, T)
		} else {
			d.mu.RLock()
			child := d.tiles[c]
			d.mu.RUnlock()
			if child != nil {
				if _, n, _, _ := child.state(); n > 0 {
					kids[i], err = p.read(ctx, d, child, core.Version(n))
				}
			}
		}
		if err != nil {
			return err
		}
	}
	next := downsample(kids)
	st.Built++

	sr, n, _, _ := ct.state()
	var prev *core.TileImage
	if n > 0 {
		var err error
		if prev, err = p.read(ctx, d, ct, core.Version(n)); err != nil {
			return err
		}
		if prev.Equal(next) {
			ct.cleanT = T
			if ct.descT <= T {
				delete(d.dirty, ct.coord)
			}
			st.Unchanged++
			return nil
		}
	}
	if sr == nil {
		var err error
		if sr, err = p.series.Open(ctx, seriesName(d.id, ct.coord), true); err != nil {
			return err
		}
		ct.mu.Lock()
		ct.sr = sr
		ct.mu.Unlock()
	}

	rec := core.Record{RecordMeta: core.RecordMeta{TStart: T, TLen: 0}}
	var payload []byte
	if prev != nil && ct.sinceBase < p.opt.MaxChain {
		raw, changed := appendDelta(nil, prev, next)
		if changed*2 < core.TilePixelCount {
			payload = p.enc.EncodeAll(raw, nil)
			if float64(ct.deltaBytes+int64(len(payload))) > p.opt.SnapshotRatio*float64(ct.baseBytes) {
				payload = nil
			}
		}
	}
	if payload != nil {
		rec.Kind, rec.Flags = core.RecordDelta, core.FlagWide
	} else {
		rec.Kind = core.RecordSnapshot
		payload = p.enc.EncodeAll(appendSnapshot(nil, next), nil)
	}
	rec.Payload = payload
	seq, err := sr.Append(ctx, rec)
	if err != nil {
		return err
	}
	if seq != n {
		return fmt.Errorf("series %s appended record %d, expected %d", seriesName(d.id, ct.coord), seq, n)
	}
	if rec.Kind == core.RecordSnapshot {
		ct.sinceBase, ct.deltaBytes, ct.baseBytes = 0, 0, int64(len(payload))
		st.Snapshots++
		st.SnapshotBytes += int64(len(payload))
	} else {
		ct.sinceBase++
		ct.deltaBytes += int64(len(payload))
		st.Deltas++
	}
	st.Versions++
	st.Bytes += int64(len(payload))
	p.cache.Put(imgKey{d: d, tile: ct.coord, v: core.Version(n + 1)}, next)

	ct.mu.Lock()
	ct.n = n + 1
	ct.lastT = T
	if n == 0 {
		ct.firstT = T
	}
	ct.mu.Unlock()
	if ct.descT <= T {
		delete(d.dirty, ct.coord)
	}
	return nil
}
