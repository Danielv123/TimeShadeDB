package pyramid

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/memstore"
)

// TestLazyLoad reopens a built pyramid with Options.Lazy and reads it from several
// goroutines while Preload runs: every read sees the same tiles as before.
func TestLazyLoad(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	ss := memstore.NewSeriesStore()
	opt := Options{MinZ: -4, Intervals: []core.TimeMs{1}}
	p1 := must[*Pyramid](t)(New(ctx, ter, ss, opt))
	replayLive(t, ter, p1, genHistory(21, 150), 0)
	if _, err := p1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[int8][]core.TileInfo{}
	for z := int8(-1); z >= opt.MinZ; z-- {
		want[z] = must[[]core.TileInfo](t)(p1.ListTiles(ctx, ds, z))
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	for round := range 3 {
		lopt := opt
		lopt.Lazy = true
		p := newPyramid(t, ter, ss, lopt)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for g := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				z := int8(-1 - (g+round)%int(-opt.MinZ))
				got, err := p.ListTiles(ctx, ds, z)
				if err != nil {
					errs <- err
					return
				}
				if !slices.Equal(got, want[z]) {
					errs <- errors.New("lazy ListTiles differs")
					return
				}
				for _, ti := range got {
					st, err := p.VersionAt(ctx, ds, ti.Tile, 1<<62)
					if err != nil || st.Version != ti.Version {
						errs <- errors.New("lazy VersionAt differs")
						return
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Preload(ctx); err != nil {
				errs <- err
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		p.mu.RLock()
		left := len(p.lazy)
		p.mu.RUnlock()
		if left != 0 {
			t.Fatalf("%d datastores still deferred after Preload", left)
		}
	}
}

// failingSeries fails List while fail is set and counts the calls.
type failingSeries struct {
	core.SeriesStore
	fail  atomic.Bool
	lists atomic.Int64
}

func (f *failingSeries) List(ctx context.Context, prefix string) ([]string, error) {
	f.lists.Add(1)
	if f.fail.Load() {
		return nil, errors.New("test: list failed")
	}
	return f.SeriesStore.List(ctx, prefix)
}

// TestLazyLoadFailure: a deferred datastore whose load fails returns the error to reads
// until the retry time, without loading again on every request.
func TestLazyLoadFailure(t *testing.T) {
	ctx := context.Background()
	ter := memstore.NewTerrain()
	ss := &failingSeries{SeriesStore: memstore.NewSeriesStore()}
	opt := Options{MinZ: -3, Intervals: []core.TimeMs{1}}
	p1 := must[*Pyramid](t)(New(ctx, ter, ss, opt))
	replayLive(t, ter, p1, genHistory(22, 60), 0)
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}
	opt.Lazy = true
	p := newPyramid(t, ter, ss, opt)
	ss.fail.Store(true)
	ss.lists.Store(0)
	for range 50 {
		if _, err := p.ListTiles(ctx, ds, -1); err == nil {
			t.Fatal("ListTiles succeeded although the load failed")
		}
	}
	if n := ss.lists.Load(); n != 1 {
		t.Fatalf("a failed load ran %d times", n)
	}
	// After the retry time the load runs again and succeeds.
	ss.fail.Store(false)
	p.mu.Lock()
	f := p.lazyErr[ds]
	f.until = time.Now().Add(-time.Second)
	p.lazyErr[ds] = f
	p.mu.Unlock()
	if tiles, err := p.ListTiles(ctx, ds, -1); err != nil || len(tiles) == 0 {
		t.Fatalf("retry: %d tiles, %v", len(tiles), err)
	}
	p.mu.RLock()
	_, still := p.lazyErr[ds]
	p.mu.RUnlock()
	if still {
		t.Fatal("failure kept after a successful load")
	}
}
