package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"timeshadedb/internal/core"
)

// Overlays is the reference core.OverlayStore.
type Overlays struct {
	mu     sync.RWMutex
	closed bool
	ds     map[core.DatastoreKey]*overlayDS
}

var _ core.OverlayStore = (*Overlays)(nil)

type kindKey struct {
	kind core.IntervalKind
	key  string
}

type overlayDS struct {
	intervals []*core.Interval
	open      map[kindKey]*core.Interval
	versions  map[core.TileCoord]uint64
}

// NewOverlays returns an empty store.
func NewOverlays() *Overlays {
	return &Overlays{ds: map[core.DatastoreKey]*overlayDS{}}
}

func (d *overlayDS) bump(iv *core.Interval) {
	d.versions[core.TileAt(iv.X, iv.Y, 0)]++
}

// Apply implements core.OverlayStore.
func (o *Overlays) Apply(ctx context.Context, events []core.OverlayEvent) (core.ApplyResult, error) {
	var res core.ApplyResult
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return res, core.ErrClosed
	}
	for _, ev := range events {
		iv := ev.Interval
		if ev.DS.Validate() != nil || !iv.Kind.Valid() || iv.Key == "" {
			res.Invalid++
			continue
		}
		if !ev.Close && !core.TileAt(iv.X, iv.Y, 0).Valid() {
			res.Invalid++
			continue
		}
		d := o.ds[ev.DS]
		if d == nil {
			d = &overlayDS{open: map[kindKey]*core.Interval{}, versions: map[core.TileCoord]uint64{}}
			o.ds[ev.DS] = d
		}
		k := kindKey{iv.Kind, iv.Key}
		cur := d.open[k]
		if cur != nil && ev.T < cur.Start {
			res.Late++
			continue
		}
		res.Accepted++
		if ev.Close {
			if cur != nil {
				cur.End = ev.T
				delete(d.open, k)
				d.bump(cur)
			}
			continue
		}
		if cur != nil {
			if cur.SameContent(iv) {
				continue
			}
			cur.End = ev.T
			delete(d.open, k)
			d.bump(cur)
		}
		n := iv
		n.Start, n.End = ev.T, 0
		d.intervals = append(d.intervals, &n)
		d.open[k] = &n
		d.bump(&n)
	}
	return res, nil
}

// Query implements core.OverlayStore.
func (o *Overlays) Query(ctx context.Context, ds core.DatastoreKey, q core.OverlayQuery) ([]core.Interval, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return nil, core.ErrClosed
	}
	if q.Tile.Z != 0 || !q.Tile.Valid() {
		return nil, fmt.Errorf("%w: %v", core.ErrBadTile, q.Tile)
	}
	t1 := q.T1
	if t1 <= q.T0 {
		t1 = q.T0 + 1
	}
	kinds := map[core.IntervalKind]bool{}
	for _, k := range q.Kinds {
		kinds[k] = true
	}
	out := []core.Interval{}
	d := o.ds[ds]
	if d == nil {
		return out, nil
	}
	for _, iv := range d.intervals {
		if len(kinds) > 0 && !kinds[iv.Kind] {
			continue
		}
		if !iv.Overlaps(q.T0, t1) || core.TileAt(iv.X, iv.Y, 0) != q.Tile {
			continue
		}
		out = append(out, *iv)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Start < b.Start
	})
	return out, nil
}

// Version implements core.OverlayStore.
func (o *Overlays) Version(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord) (uint64, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return 0, core.ErrClosed
	}
	if tile.Z != 0 || !tile.Valid() {
		return 0, fmt.Errorf("%w: %v", core.ErrBadTile, tile)
	}
	if d := o.ds[ds]; d != nil {
		return d.versions[tile], nil
	}
	return 0, nil
}

// Close implements core.OverlayStore.
func (o *Overlays) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	return nil
}
