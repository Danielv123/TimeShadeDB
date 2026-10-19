// Area replay ported from illisis-minimap shared/replay.ts.
// Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
)

// replayArea returns an area's pixels at time t the way illisis's viewer and store
// rebuild it (replay.ts): the latest snapshot at or before t, then the records of the
// change files after it up to t.
func replayArea(dir string, idx areaIndexJSON, t int64) ([]uint16, error) {
	snap := int64(math.MinInt64)
	for _, s := range idx.Snaps {
		if s <= t {
			snap = s
		}
	}
	var px []uint16
	if snap != math.MinInt64 {
		b, err := readGz(snapPath(dir, snap), maxChangeBytes)
		if err != nil {
			return nil, err
		}
		if px, err = DecodeSnap(b); err != nil {
			return nil, fmt.Errorf("%s: %w", snapPath(dir, snap), err)
		}
	} else {
		px = make([]uint16, AreaSize*AreaSize)
	}
	stop := errors.New("stop")
	for _, c := range idx.Changes {
		if c[0] > t {
			break
		}
		if c[0] <= snap {
			continue
		}
		b, err := readGz(changePath(dir, c[0]), maxChangeBytes)
		if err != nil {
			return nil, err
		}
		err = walkChange(c[0], b, func(rt int64, block uint8, n int, data []byte) error {
			if rt > t {
				return stop
			}
			applyArea(px, block, n, data)
			return nil
		})
		if err == stop {
			return px, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", changePath(dir, c[0]), err)
		}
	}
	return px, nil
}

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// HistoryPerArea bounds the historical checks per z0 area: each check compares the
	// store at the end of the terrain window holding an illisis snapshot with illisis's
	// replay just before that time. 0 checks none, < 0 checks every snapshot, n > 0
	// checks n snapshots spread evenly (always including the latest).
	HistoryPerArea int
	// Workers bounds parallel area checks (default GOMAXPROCS).
	Workers int
}

// AreaCheck is one failed comparison.
type AreaCheck struct {
	DS   string         `json:"ds"`
	Tile core.TileCoord `json:"tile"`
	// What: "final", "history@<t>" or "coarse".
	What string `json:"what"`
	// Diff is the number of differing pixels; -1 when a side could not be read.
	Diff  int    `json:"diff"`
	Error string `json:"error,omitempty"`
}

// TerrainVerify summarises Verify for terrain.
type TerrainVerify struct {
	// FinalAreas z0 areas were compared at their final state; FinalEqual of them matched
	// bit for bit.
	FinalAreas int `json:"final_areas"`
	FinalEqual int `json:"final_equal"`
	// LatestSnapshotAreas had a latest snapshot after their last record, so the final
	// comparison was against that snapshot directly; the others were compared with the
	// latest snapshot plus the change files after it.
	LatestSnapshotAreas int `json:"latest_snapshot_areas"`
	// HistoryChecks compared the store at a window end with illisis's replay;
	// HistoryEqual matched.
	HistoryChecks int `json:"history_checks"`
	HistoryEqual  int `json:"history_equal"`
	// CoarseAreas illisis z1-z4 areas were compared, at their final state, with the 2x2
	// average (core.Downsample) of the store's final z0 tiles; CoarseEqual matched.
	CoarseAreas int         `json:"coarse_areas"`
	CoarseEqual int         `json:"coarse_equal"`
	Failures    []AreaCheck `json:"failures,omitempty"`
	Ms          int64       `json:"ms"`
}

// OK reports whether every check passed.
func (v *TerrainVerify) OK() bool {
	return v.FinalEqual == v.FinalAreas && v.HistoryEqual == v.HistoryChecks && v.CoarseEqual == v.CoarseAreas
}

func diffPix(a, b []uint16) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// VerifyTerrain compares the store with the illisis data: every z0 area's final state,
// optionally historical states at snapshot times, and every coarse area against the
// pyramid average of the final z0 state.
func VerifyTerrain(ctx context.Context, d *Dataset, store core.TerrainStore, filter Options, vo VerifyOptions) (*TerrainVerify, error) {
	start := time.Now()
	if vo.Workers <= 0 {
		vo.Workers = filter.workersOrDefault()
	}
	out := &TerrainVerify{}
	var mu sync.Mutex
	fail := func(c AreaCheck) {
		if len(out.Failures) < 100 {
			out.Failures = append(out.Failures, c)
		}
	}
	far := core.TimeMs(math.MaxInt64)
	for _, src := range d.Datastores {
		if len(src.Areas) == 0 || !filter.wantDS(src.Key) {
			continue
		}
		info, err := datastoreInfo(ctx, store, src.Key)
		if err != nil {
			return out, err
		}
		w := info.Window
		final := map[core.TileCoord]*core.TileImage{}
		sem := make(chan struct{}, vo.Workers)
		var wg sync.WaitGroup
		for _, a := range src.Areas {
			wg.Add(1)
			sem <- struct{}{}
			go func(a SourceArea) {
				defer wg.Done()
				defer func() { <-sem }()
				res := checkArea(ctx, src.Key, a, store, w, far, vo.HistoryPerArea)
				mu.Lock()
				defer mu.Unlock()
				out.FinalAreas++
				if res.finalImg != nil {
					final[a.Tile] = res.finalImg
				}
				if res.latestSnap {
					out.LatestSnapshotAreas++
				}
				out.HistoryChecks += res.histChecks
				out.HistoryEqual += res.histEqual
				if res.finalOK {
					out.FinalEqual++
				}
				for _, f := range res.failures {
					fail(f)
				}
			}(a)
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// Coarse levels: illisis level k is v3 z = -k on the same grid.
		memo := map[core.TileCoord]*core.TileImage{}
		var pyr func(t core.TileCoord) *core.TileImage
		pyr = func(t core.TileCoord) *core.TileImage {
			if t.Z == 0 {
				return final[t]
			}
			if im, ok := memo[t]; ok {
				return im
			}
			var kids [4]*core.TileImage
			has := false
			for i, c := range t.Children() {
				kids[i] = pyr(c)
				has = has || kids[i] != nil
			}
			var im *core.TileImage
			if has {
				im = core.Downsample(kids)
			}
			memo[t] = im
			return im
		}
		for k := range src.Coarse {
			for _, a := range src.Coarse[k] {
				out.CoarseAreas++
				idx, err := readAreaIndex(a.Dir)
				var px []uint16
				if err == nil {
					px, err = replayArea(a.Dir, idx, math.MaxInt64)
				}
				if err != nil {
					fail(AreaCheck{DS: src.Key.ID(), Tile: a.Tile, What: "coarse", Diff: -1, Error: err.Error()})
					continue
				}
				im := pyr(a.Tile)
				if im == nil {
					im = core.NewTileImage()
				}
				if n := diffPix(px, im.Pix); n == 0 {
					out.CoarseEqual++
				} else {
					fail(AreaCheck{DS: src.Key.ID(), Tile: a.Tile, What: "coarse", Diff: n})
				}
			}
		}
	}
	sort.Slice(out.Failures, func(i, j int) bool {
		a, b := out.Failures[i], out.Failures[j]
		if a.DS != b.DS {
			return a.DS < b.DS
		}
		return a.Tile.String() < b.Tile.String()
	})
	out.Ms = time.Since(start).Milliseconds()
	return out, nil
}

func (o Options) workersOrDefault() int {
	o.defaults()
	return o.Workers
}

func datastoreInfo(ctx context.Context, store core.TerrainStore, key core.DatastoreKey) (core.DatastoreInfo, error) {
	list, err := store.Datastores(ctx)
	if err != nil {
		return core.DatastoreInfo{}, err
	}
	for _, info := range list {
		if info.Key == key {
			return info, nil
		}
	}
	return core.DatastoreInfo{}, fmt.Errorf("%w: %v", core.ErrUnknownDatastore, key)
}

type areaResult struct {
	finalImg              *core.TileImage
	finalOK, latestSnap   bool
	histChecks, histEqual int
	failures              []AreaCheck
}

func checkArea(ctx context.Context, key core.DatastoreKey, a SourceArea, store core.TerrainStore, w, far core.TimeMs, history int) areaResult {
	var res areaResult
	id := key.ID()
	idx, err := readAreaIndex(a.Dir)
	if err != nil {
		res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: "final", Diff: -1, Error: err.Error()})
		return res
	}
	want, err := replayArea(a.Dir, idx, math.MaxInt64)
	if err != nil {
		res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: "final", Diff: -1, Error: err.Error()})
		return res
	}
	if n := len(idx.Snaps); n > 0 {
		last := idx.Snaps[n-1]
		res.latestSnap = len(idx.Changes) == 0 || idx.Changes[len(idx.Changes)-1][0] <= last
	}
	got, _, err := store.ReadTile(ctx, key, a.Tile, far, false)
	switch {
	case errors.Is(err, core.ErrNoTile):
		got = core.NewTileImage()
	case err != nil:
		res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: "final", Diff: -1, Error: err.Error()})
		return res
	}
	res.finalImg = got
	if n := diffPix(want, got.Pix); n == 0 {
		res.finalOK = true
	} else {
		res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: "final", Diff: n})
	}
	for _, ts := range pickSnaps(idx.Snaps, history) {
		if ctx.Err() != nil {
			return res
		}
		t := core.WindowEnd(core.TimeMs(ts), w)
		res.histChecks++
		what := fmt.Sprintf("history@%d", int64(t))
		want, err := replayArea(a.Dir, idx, int64(t)-1)
		if err != nil {
			res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: what, Diff: -1, Error: err.Error()})
			continue
		}
		st, err := store.VersionAt(ctx, key, a.Tile, t)
		var got *core.TileImage
		if err == nil {
			got, err = store.ReadVersion(ctx, key, a.Tile, st.Version)
		}
		if err != nil {
			res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: what, Diff: -1, Error: err.Error()})
			continue
		}
		if n := diffPix(want, got.Pix); n == 0 {
			res.histEqual++
		} else {
			res.failures = append(res.failures, AreaCheck{DS: id, Tile: a.Tile, What: what, Diff: n})
		}
	}
	return res
}

// pickSnaps chooses up to n snapshot times spread evenly, always including the latest.
func pickSnaps(snaps []int64, n int) []int64 {
	if n == 0 || len(snaps) == 0 {
		return nil
	}
	if n < 0 || n >= len(snaps) {
		return snaps
	}
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, snaps[(len(snaps)-1)-i*(len(snaps)-1)/max(n-1, 1)])
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MoverVerify is the read-back check of one surface: the store's closed windows
// counted the same way as MoverReport.Imported.
type MoverVerify struct {
	Surface string `json:"surface"`
	// Stored counts every row of every closed window.
	Stored    MoverCounts `json:"stored"`
	Handoffs  int         `json:"handoffs"`
	Keyframes int         `json:"keyframes"`
	Windows   int         `json:"windows"`
	// Match is true when Stored and Handoffs equal the import report's Imported and
	// Handoffs.
	Match bool `json:"match"`
}

// VerifyMovers reads every closed window back from the store and compares the counts
// with the import report.
func VerifyMovers(ctx context.Context, rep *Report, store core.MoverStore) ([]MoverVerify, error) {
	infos, err := store.Surfaces(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]core.SurfaceInfo{}
	for _, in := range infos {
		byID[in.ID] = in
	}
	var out []MoverVerify
	for _, mr := range rep.Movers {
		v := MoverVerify{Surface: mr.Surface, Stored: newCounts()}
		info, ok := byID[mr.Surface]
		if ok {
			journeys := map[string]core.MoverKind{}
			for _, day := range info.Days {
				idx, err := store.Day(ctx, info.Key, day)
				if errors.Is(err, core.ErrNoVersion) {
					continue
				}
				if err != nil {
					return out, err
				}
				v.Keyframes += len(idx.Keyframes)
				for _, ref := range idx.Windows {
					win, err := store.Window(ctx, info.Key, ref.Start)
					if err != nil {
						return out, err
					}
					v.Windows++
					for _, r := range win.Rows {
						m := win.Movers[r.Mover]
						if _, seen := journeys[m.ID]; !seen {
							journeys[m.ID] = m.Kind
						}
						if r.Flags&core.FlagGone != 0 {
							v.Stored.Gones[string(m.Kind)]++
							if r.Flags&core.FlagHandoff != 0 {
								v.Handoffs++
							}
						} else {
							v.Stored.Samples[string(m.Kind)]++
						}
					}
				}
			}
			for _, k := range journeys {
				v.Stored.Journeys[string(k)]++
			}
		}
		v.Match = countsEqual(v.Stored, mr.Imported) && v.Handoffs == mr.Handoffs
		out = append(out, v)
	}
	return out, nil
}

func countsEqual(a, b MoverCounts) bool {
	eq := func(x, y map[string]int) bool {
		for k, v := range x {
			if y[k] != v {
				return false
			}
		}
		for k, v := range y {
			if x[k] != v {
				return false
			}
		}
		return true
	}
	return eq(a.Journeys, b.Journeys) && eq(a.Samples, b.Samples) && eq(a.Gones, b.Gones)
}

// OverlayVerify is the read-back check of one datastore's overlays.
type OverlayVerify struct {
	DS        string `json:"ds"`
	Intervals int    `json:"intervals"`
	Match     bool   `json:"match"`
}

// VerifyOverlays queries every tile that holds an imported version over the whole
// imported time range and compares the interval count with the report.
func VerifyOverlays(ctx context.Context, rep *Report, store core.OverlayStore) ([]OverlayVerify, error) {
	var out []OverlayVerify
	for _, or := range rep.Overlays {
		key, err := core.ParseDatastoreID(or.DS)
		if err != nil {
			return out, err
		}
		v := OverlayVerify{DS: or.DS}
		for tile := range or.tiles {
			// Versions of other forces in this file (none in practice) live in their own
			// datastore; they are not counted here.
			list, err := store.Query(ctx, key, core.OverlayQuery{Tile: tile, T0: or.first, T1: or.last + 1})
			if err != nil {
				return out, err
			}
			v.Intervals += len(list)
		}
		v.Match = v.Intervals == or.Intervals
		out = append(out, v)
	}
	return out, nil
}
