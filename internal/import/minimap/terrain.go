package minimap

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"timeshadedb/internal/v3/core"
)

// areaIndexJSON is an area's index.json.gz (illisis keys.ts AreaIndex).
type areaIndexJSON struct {
	Snaps   []int64    `json:"snaps"`
	Changes [][2]int64 `json:"changes"`
}

func readAreaIndex(dir string) (areaIndexJSON, error) {
	var idx areaIndexJSON
	b, err := readGz(filepath.Join(dir, "index.json.gz"), maxJSONBytes)
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return idx, fmt.Errorf("%s: %w: %v", dir, ErrFormat, err)
	}
	keep := idx.Changes[:0]
	for _, c := range idx.Changes {
		if saneTime(c[0]) {
			keep = append(keep, c)
		}
	}
	idx.Changes = keep
	sort.SliceStable(idx.Changes, func(i, j int) bool { return idx.Changes[i][0] < idx.Changes[j][0] })
	snaps := idx.Snaps[:0]
	for _, s := range idx.Snaps {
		if saneTime(s) {
			snaps = append(snaps, s)
		}
	}
	idx.Snaps = snaps
	sort.Slice(idx.Snaps, func(i, j int) bool { return idx.Snaps[i] < idx.Snaps[j] })
	return idx, nil
}

func changePath(dir string, t int64) string {
	return filepath.Join(dir, strconv.FormatInt(t, 10)+".c.gz")
}

func snapPath(dir string, t int64) string {
	return filepath.Join(dir, strconv.FormatInt(t, 10)+".s.gz")
}

// TerrainReport counts what the terrain import read and wrote for one datastore.
type TerrainReport struct {
	DS          string `json:"ds"`
	Areas       int    `json:"areas"`
	ChangeFiles int    `json:"change_files"`
	// Records are MMC1 records read; WholeBlocks of them carried a whole block.
	Records     int   `json:"records"`
	WholeBlocks int   `json:"whole_blocks"`
	PixelWrites int64 `json:"pixel_writes"`
	// Backwards counts records older than the previous record of their area; they are
	// applied at the previous record's time (none in the test data).
	Backwards int `json:"backwards"`
	// Updates are the ChunkUpdates handed to the store: one per (chunk, window) with at
	// least one record. Deletes of them had all pixels 0.
	Updates int `json:"updates"`
	Deletes int `json:"deletes"`
	// Windows is the number of distinct terrain windows with updates.
	Windows int         `json:"windows"`
	FirstT  core.TimeMs `json:"first"`
	LastT   core.TimeMs `json:"last"`
	// ErrorCount files failed to read or decode; Errors lists the first 20.
	ErrorCount int      `json:"error_count"`
	Errors     []string `json:"errors,omitempty"`
	windows    map[core.TimeMs]struct{}
	mu         sync.Mutex
}

func (r *TerrainReport) addErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ErrorCount++
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, err.Error())
	}
}

// areaWindow is one area's updates for one terrain window.
type areaWindow struct {
	start   core.TimeMs
	updates []core.ChunkUpdate
}

// areaStats are per-area counters, merged into the TerrainReport at the end.
type areaStats struct {
	files, records, whole, backwards, updates, deletes int
	pixels                                             int64
	first, last                                        core.TimeMs
}

// runArea replays one z0 area's change files and sends, in time order, one areaWindow
// per terrain window holding the state of every chunk touched in that window after its
// last record. sem bounds concurrent decoding; it is never held while sending.
func runArea(ctx context.Context, ds core.DatastoreKey, area SourceArea, w core.TimeMs, sem chan struct{}, out chan<- areaWindow, rep *TerrainReport, st *areaStats) {
	defer close(out)
	idx, err := readAreaIndex(area.Dir)
	if err != nil {
		rep.addErr(err)
		return
	}
	var (
		state   [core.TileBlocks][BlockPixels]uint16
		lastT   [core.TileBlocks]core.TimeMs
		touched []uint8
		isTouch [core.TileBlocks]bool
		cur     core.TimeMs
		prevT   core.TimeMs
		started bool
	)
	flush := func(pending []areaWindow) []areaWindow {
		if len(touched) == 0 {
			return pending
		}
		sort.Slice(touched, func(i, j int) bool { return touched[i] < touched[j] })
		ups := make([]core.ChunkUpdate, 0, len(touched))
		for _, b := range touched {
			u := core.ChunkUpdate{DS: ds, Chunk: area.Tile.Chunk(b), T: lastT[b]}
			empty := true
			for _, v := range state[b] {
				if v != 0 {
					empty = false
					break
				}
			}
			if empty {
				u.Deleted = true
				st.deletes++
			} else {
				px := core.ChunkPixels(state[b])
				u.Pixels = &px
			}
			ups = append(ups, u)
			isTouch[b] = false
		}
		st.updates += len(ups)
		touched = touched[:0]
		return append(pending, areaWindow{start: cur, updates: ups})
	}
	for _, c := range idx.Changes {
		if ctx.Err() != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var pending []areaWindow
		b, err := readGz(changePath(area.Dir, c[0]), maxChangeBytes)
		if err == nil {
			st.files++
			err = walkChange(c[0], b, func(t int64, block uint8, n int, data []byte) error {
				tm := core.TimeMs(t)
				if started && tm < prevT {
					st.backwards++
					tm = prevT
				}
				ws := core.WindowStart(tm, w)
				if !started {
					cur, started = ws, true
					st.first = tm
				} else if ws != cur {
					pending = flush(pending)
					cur = ws
				}
				applyRecord(&state[block], n, data)
				if !isTouch[block] {
					isTouch[block] = true
					touched = append(touched, block)
				}
				lastT[block] = tm
				prevT = tm
				st.records++
				if n == 0 {
					st.whole++
					st.pixels += BlockPixels
				} else {
					st.pixels += int64(n)
				}
				return nil
			})
		}
		<-sem
		if err != nil {
			rep.addErr(fmt.Errorf("%v %v: %w", ds, area.Tile, err))
		}
		for _, aw := range pending {
			select {
			case out <- aw:
			case <-ctx.Done():
				return
			}
		}
	}
	st.last = prevT
	if pending := flush(nil); len(pending) > 0 {
		select {
		case out <- pending[0]:
		case <-ctx.Done():
		}
	}
}

// windowHeap orders area heads by window start, then by area order.
type windowHead struct {
	area int
	win  areaWindow
}

type windowHeap []windowHead

func (h windowHeap) Len() int { return len(h) }
func (h windowHeap) Less(i, j int) bool {
	if h[i].win.start != h[j].win.start {
		return h[i].win.start < h[j].win.start
	}
	return h[i].area < h[j].area
}
func (h windowHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *windowHeap) Push(x any)   { *h = append(*h, x.(windowHead)) }
func (h *windowHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// importTerrain feeds every z0 area of every datastore into the store in global window
// order, committing every commitEvery of map time and once at the end.
func importTerrain(ctx context.Context, d *Dataset, opt *Options, store core.TerrainStore) ([]*TerrainReport, core.ApplyResult, error) {
	var res core.ApplyResult
	w := opt.Window
	type areaRef struct {
		rep *TerrainReport
		st  *areaStats
		ch  chan areaWindow
	}
	var reports []*TerrainReport
	var areas []areaRef
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, opt.Workers)
	var wg sync.WaitGroup
	for _, src := range d.Datastores {
		if len(src.Areas) == 0 || !opt.wantDS(src.Key) {
			continue
		}
		if err := store.EnsureDatastore(ctx, src.Key, w); err != nil {
			return nil, res, err
		}
		rep := &TerrainReport{DS: src.Key.ID(), Areas: len(src.Areas), windows: map[core.TimeMs]struct{}{}}
		reports = append(reports, rep)
		for _, a := range src.Areas {
			ref := areaRef{rep: rep, st: &areaStats{}, ch: make(chan areaWindow, 16)}
			areas = append(areas, ref)
			wg.Add(1)
			go func(key core.DatastoreKey, a SourceArea) {
				defer wg.Done()
				runArea(ctx, key, a, w, sem, ref.ch, rep, ref.st)
			}(src.Key, a)
		}
	}
	h := &windowHeap{}
	for i, a := range areas {
		if aw, ok := <-a.ch; ok {
			heap.Push(h, windowHead{area: i, win: aw})
		}
	}
	var (
		batch      []core.ChunkUpdate
		lastCommit core.TimeMs
		lastEnd    core.TimeMs
		haveCommit bool
	)
	for h.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return reports, res, err
		}
		start := (*h)[0].win.start
		batch = batch[:0]
		for h.Len() > 0 && (*h)[0].win.start == start {
			hd := heap.Pop(h).(windowHead)
			batch = append(batch, hd.win.updates...)
			areas[hd.area].rep.windows[start] = struct{}{}
			if aw, ok := <-areas[hd.area].ch; ok {
				heap.Push(h, windowHead{area: hd.area, win: aw})
			}
		}
		r, err := store.Apply(ctx, batch)
		res.Add(r)
		if err != nil {
			return reports, res, err
		}
		end := start + w
		lastEnd = end
		if !haveCommit {
			lastCommit, haveCommit = start, true
		}
		if end-lastCommit >= opt.CommitEvery {
			if err := store.Commit(ctx, end); err != nil {
				return reports, res, err
			}
			lastCommit = end
		}
	}
	wg.Wait()
	if haveCommit {
		if err := store.Commit(ctx, lastEnd); err != nil {
			return reports, res, err
		}
	}
	for _, a := range areas {
		r, st := a.rep, a.st
		r.ChangeFiles += st.files
		r.Records += st.records
		r.WholeBlocks += st.whole
		r.PixelWrites += st.pixels
		r.Backwards += st.backwards
		r.Updates += st.updates
		r.Deletes += st.deletes
		if st.records > 0 {
			if r.FirstT == 0 || st.first < r.FirstT {
				r.FirstT = st.first
			}
			if st.last > r.LastT {
				r.LastT = st.last
			}
		}
	}
	for _, r := range reports {
		r.Windows = len(r.windows)
	}
	return reports, res, nil
}
