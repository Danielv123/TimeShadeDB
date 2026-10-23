package pyramid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"timeshadedb/internal/core"
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
	// Syncs counts series synced before their new versions were published.
	Syncs int `json:"syncs"`
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
	s.Syncs += o.Syncs
	s.Bytes += o.Bytes
	s.SnapshotBytes += o.SnapshotBytes
}

// Update builds the coarse levels that are due, for every datastore, at T = the
// datastore's horizon (live mode), and refreshes the live latest of every coarse tile
// whose z0 descendants changed. Call it after each terrain Commit; it is cheap when
// nothing changed (O(z0 tiles) bookkeeping, no pixels read). Coarse answers are final
// only below BuiltUpTo, which Update advances to the horizon it read when its build
// pass succeeds. A datastore whose pass fails does not stop the others; the next Update
// retries it.
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
	var errs []error
	for _, info := range infos {
		if info.Tiles == 0 || info.Horizon == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		d, err := p.ensureDS(ctx, info.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tiles, err := p.terrain.ListTiles(ctx, info.Key, 0)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, ti := range tiles {
			if ti.Version > 0 {
				p.propagate(d, ti.Tile, ti.LastT)
			}
		}
		T, prev := info.Horizon, d.builtT
		d.mu.Lock()
		d.seenT = max(d.seenT, T)
		d.mu.Unlock()
		if prev == math.MinInt64 || prev >= T {
			prev = T - 1 // first pass (or no progress): due only at an exact multiple
		}
		st, err := p.pass(ctx, d, T, prev, force)
		total.add(st)
		if err != nil {
			// Keep builtT: the levels due in this pass stay due for the next Update.
			errs = append(errs, err)
			continue
		}
		d.mu.Lock()
		d.builtT = max(d.builtT, T)
		d.mu.Unlock()
	}
	return total, errors.Join(errs...)
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
		if ct.bn > 0 {
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
			// the change, and a z0 tile's first version creates its coarse ancestors
			// at once (tiles without versions are always due). Passes at those
			// instants (below the horizon; the final flush covers the rest) give the
			// same versions.
			if i == 0 {
				passSet[pt.T] = struct{}{}
			}
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
	total.add(st)
	if err != nil {
		return total, err
	}
	d.mu.Lock()
	d.builtT = max(d.builtT, horizon)
	d.seenT = max(d.seenT, horizon)
	d.mu.Unlock()
	return total, nil
}

// propagate records that z0 tile has versions up to time lastT and marks every coarse
// ancestor whose content may have changed as dirty (and its live latest as changed).
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
			ct = newTile(a)
			d.mu.Lock()
			d.tiles[a] = ct
			d.mu.Unlock()
		}
		ct.mu.Lock()
		if lastT > ct.descT {
			ct.descT = lastT
			d.liveSeq++
			ct.liveSeq = d.liveSeq
		}
		dirty := ct.bn == 0 || ct.descT > max(ct.blast, ct.cleanT)
		ct.mu.Unlock()
		if dirty {
			d.dirty[a] = struct{}{}
		}
	}
}

// pass builds, at time T, every dirty tile of every level that is due: level k = -z is
// due when a multiple of its interval lies in (prev, T], or always with force. Since
// each interval is a multiple of the finer one, levels are due from k = 1 up to some k.
// Tiles without any version are built at every pass once their children are current.
// Each level's new versions are synced before they are published, and before the next
// level reads them.
func (p *Pyramid) pass(ctx context.Context, d *dsState, T, prev core.TimeMs, force bool) (BuildStats, error) {
	if len(d.dirty) == 0 {
		return BuildStats{}, nil
	}
	st := BuildStats{Passes: 1}
	due := make([]bool, p.levels()+1)
	for k := 1; k <= p.levels(); k++ {
		iv := int64(p.interval(k))
		due[k] = force || core.FloorDiv(int64(T), iv) > core.FloorDiv(int64(prev), iv)
	}
	byLevel := make([][]*ctile, p.levels()+1)
	d.mu.RLock()
	for c := range d.dirty {
		k := -int(c.Z)
		if ct := d.tiles[c]; ct != nil && (due[k] || ct.bn == 0) {
			byLevel[k] = append(byLevel[k], ct)
		}
	}
	d.mu.RUnlock()
	for k := 1; k <= p.levels(); k++ {
		todo := byLevel[k]
		if !due[k] {
			// Only new tiles; their children must be current at T (they normally are:
			// a new tile's children are new too and were built just before).
			todo = slices.DeleteFunc(todo, func(ct *ctile) bool { return !p.childrenCurrent(d, ct.coord) })
		}
		if len(todo) == 0 {
			continue
		}
		slices.SortFunc(todo, func(a, b *ctile) int {
			if a.coord.Y != b.coord.Y {
				return int(a.coord.Y) - int(b.coord.Y)
			}
			return int(a.coord.X) - int(b.coord.X)
		})
		var lv levelBuild
		var err error
		for _, ct := range todo {
			if err = ctx.Err(); err != nil {
				break
			}
			if err = p.buildTile(ctx, d, ct, T, &st, &lv); err != nil {
				err = fmt.Errorf("pyramid: build %v %v at %v: %w", d.key, ct.coord, T, err)
				break
			}
		}
		if e := p.commitLevel(ctx, d, &lv, &st); err == nil && e != nil {
			err = fmt.Errorf("pyramid: sync %v level %d at %v: %w", d.key, -k, T, e)
		}
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

// childrenCurrent reports whether every coarse child of tile is built and published.
func (p *Pyramid) childrenCurrent(d *dsState, tile core.TileCoord) bool {
	if tile.Z == -1 {
		return true
	}
	for _, c := range tile.Children() {
		d.mu.RLock()
		child := d.tiles[c]
		d.mu.RUnlock()
		if child == nil {
			continue
		}
		if _, dirty := d.dirty[c]; dirty || child.bn != child.n {
			return false
		}
	}
	return true
}

// levelBuild collects the tiles of one level touched by a pass: their open series and
// the newest image appended, published after the sync.
type levelBuild struct {
	entries []*levelEntry
}

type levelEntry struct {
	ct *ctile
	sr core.Series
	im *core.TileImage // newest appended version's content, or nil
	v  core.Version
}

func (lv *levelBuild) add(ct *ctile) *levelEntry {
	e := &levelEntry{ct: ct}
	lv.entries = append(lv.entries, e)
	return e
}

// commitLevel syncs every series that has unpublished versions, then publishes them
// (cache first, then the tile's version count), and closes every handle. A tile whose
// sync fails stays dirty, so a later pass syncs and publishes it.
func (p *Pyramid) commitLevel(ctx context.Context, d *dsState, lv *levelBuild, st *BuildStats) error {
	var errs []error
	for _, e := range lv.entries {
		ct := e.ct
		if ct.bn != ct.n {
			err := func() error {
				if e.sr == nil {
					sr, err := p.series.Open(ctx, seriesName(d.id, ct.coord), true)
					if err != nil {
						return err
					}
					e.sr = sr
				}
				return e.sr.Sync()
			}()
			if err != nil {
				errs = append(errs, fmt.Errorf("%v: %w", ct.coord, err))
				d.dirty[ct.coord] = struct{}{}
			} else {
				st.Syncs++
				if e.im != nil {
					p.cache.Put(imgKey{d: d, tile: ct.coord, v: e.v}, e.im)
				}
				ct.mu.Lock()
				ct.n, ct.firstT, ct.lastT = ct.bn, ct.bfirstT, ct.blast
				ct.mu.Unlock()
			}
		}
		if e.sr != nil {
			if err := e.sr.Close(); err != nil {
				errs = append(errs, fmt.Errorf("%v: %w", ct.coord, err))
			}
		}
	}
	return errors.Join(errs...)
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
// version if it changed. The version is published by commitLevel after the sync.
func (p *Pyramid) buildTile(ctx context.Context, d *dsState, ct *ctile, T core.TimeMs, st *BuildStats, lv *levelBuild) error {
	var e *levelEntry
	open := func() (core.Series, error) {
		if e != nil && e.sr != nil {
			return e.sr, nil
		}
		sr, err := p.series.Open(ctx, seriesName(d.id, ct.coord), true)
		if err != nil {
			return nil, err
		}
		if e == nil {
			e = lv.add(ct)
		}
		e.sr = sr
		if ct.suspect || sr.Len() != ct.bn {
			if err := loadState(sr, ct); err != nil {
				return nil, err
			}
			ct.suspect = false
		}
		return sr, nil
	}
	if ct.suspect {
		if _, err := open(); err != nil {
			return err
		}
	}
	if ct.bn > 0 && T <= ct.blast {
		// Already built for T (the z0 content at T is final), or T went backwards:
		// nothing new can be said about this instant. The tile stays dirty if z0 holds
		// newer versions (an early-flushed window) and is built at a later T.
		if ct.bn != ct.n && e == nil {
			lv.add(ct)
		}
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
				if n := child.view().n; n > 0 {
					kids[i], err = p.read(ctx, d, c, nil, core.Version(n))
				}
			}
		}
		if err != nil {
			return err
		}
	}
	next := downsample(kids)
	st.Built++

	var prev *core.TileImage
	if ct.bn > 0 {
		var ok bool
		if prev, ok = p.cache.Get(imgKey{d: d, tile: ct.coord, v: core.Version(ct.bn)}); !ok {
			sr, err := open()
			if err != nil {
				return err
			}
			if prev, err = p.read(ctx, d, ct.coord, sr, core.Version(ct.bn)); err != nil {
				return err
			}
		}
		if prev.Equal(next) {
			ct.mu.Lock()
			ct.cleanT = T
			ct.mu.Unlock()
			if ct.descT <= T {
				delete(d.dirty, ct.coord)
			}
			if ct.bn != ct.n && e == nil {
				lv.add(ct) // staged versions from a failed sync: publish them now
			}
			st.Unchanged++
			return nil
		}
	}
	sr, err := open()
	if err != nil {
		return err
	}
	if ct.bn > 0 && T <= ct.blast {
		return nil // the reload found versions at or after T
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
		ct.suspect = true
		return err
	}
	if seq != ct.bn {
		ct.suspect = true
		return fmt.Errorf("series %s appended record %d, expected %d", seriesName(d.id, ct.coord), seq, ct.bn)
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
	ct.bn++
	ct.blast = T
	if ct.bn == 1 {
		ct.bfirstT = T
	}
	e.im, e.v = next, core.Version(ct.bn)
	if ct.descT <= T {
		delete(d.dirty, ct.coord)
	}
	return nil
}
