package movers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

func openTemp(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestMoverContract(t *testing.T) {
	coretest.MoverStore(t, func(t *testing.T) core.MoverStore {
		m, _ := openTemp(t, Options{SyncInterval: -1})
		return m
	})
}

// TestMoverContractLightEncoder: the same contract with the lighter window encoder.
func TestMoverContractLightEncoder(t *testing.T) {
	coretest.MoverStore(t, func(t *testing.T) core.MoverStore {
		m, _ := openTemp(t, Options{SyncInterval: -1, LightEncoder: true})
		return m
	})
}

func TestMoverContractWithFsync(t *testing.T) {
	coretest.MoverStore(t, func(t *testing.T) core.MoverStore {
		m, _ := openTemp(t, Options{})
		return m
	})
}

// TestMoverContractAfterReopen runs the contract tests against a store that is closed
// and reopened after every Commit, so every read goes through the disk state.
func TestMoverContractAfterReopen(t *testing.T) {
	coretest.MoverStore(t, func(t *testing.T) core.MoverStore {
		dir := t.TempDir()
		m, err := Open(dir, Options{SyncInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		return &reopening{t: t, dir: dir, Store: m}
	})
}

type reopening struct {
	t   *testing.T
	dir string
	*Store
}

func (r *reopening) Commit(ctx context.Context, upTo core.TimeMs) error {
	if err := r.Store.Commit(ctx, upTo); err != nil {
		return err
	}
	if err := r.Store.Close(); err != nil {
		return err
	}
	m, err := Open(r.dir, Options{SyncInterval: -1})
	if err != nil {
		return err
	}
	r.Store = m
	return nil
}

// randomSamples makes a workload with several journeys, meta changes, stops, gones,
// handoffs, carriages, duplicates and out-of-order arrival.
func randomSamples(rng *rand.Rand, t0 core.TimeMs, span core.TimeMs, n int) []core.MoverSample {
	sfcs := []core.SurfaceKey{{Map: "gw-a.1", Surface: "nauvis"}, {Map: "gw-a.1", Surface: "Gleba"}}
	var out []core.MoverSample
	for i := 0; i < n; i++ {
		j := rng.Intn(12)
		meta := core.MoverMeta{ID: fmt.Sprintf("j%d", j), Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3, Kinds: "LCC"}
		if j%3 == 1 {
			meta = core.MoverMeta{ID: fmt.Sprintf("p:%d", j), Kind: core.KindPlayer, Force: "player", Name: fmt.Sprintf("P%d", j)}
		}
		if rng.Intn(20) == 0 {
			meta.Kinds = "CCl" // meta change: reversed
		}
		s := core.MoverSample{
			Surface:     sfcs[j%2],
			Meta:        meta,
			T:           t0 + core.TimeMs(rng.Int63n(int64(span))),
			X:           rng.Float64()*2000 - 1000,
			Y:           rng.Float64()*2000 - 1000,
			Orientation: rng.Float64(),
		}
		s.Tick = int64(s.T-t0) * 60 / 1000
		switch rng.Intn(14) {
		case 0, 1:
			s.Flags = core.FlagGone
		case 2:
			s.Flags = core.FlagStop
			s.Carriages = []core.Carriage{{X: s.X, Y: s.Y, Orientation: s.Orientation}, {X: s.X + 7, Y: s.Y, Orientation: s.Orientation}}
		case 3:
			s.Flags = core.FlagEndSwitch | core.FlagHandoff // handoff is ignored on input
		case 4:
			s.Flags = core.FlagNoHeading
		}
		if meta.Kind == core.KindTrain && s.Flags&(core.FlagGone|core.FlagNoHeading) == 0 && rng.Intn(2) == 0 {
			// a rail path sample (docs/v3/RAIL-GEOMETRY.md 8)
			s.Flags |= core.FlagRail
			s.Speed = rng.Float64() * 1.3
			s.RailStart = rng.Intn(5) == 0
			for k := rng.Intn(4); k > 0; k-- {
				s.Rail = append(s.Rail, core.RailPiece{X: int32(s.X) + int32(rng.Intn(80)) - 40, Y: int32(s.Y) + int32(rng.Intn(80)) - 40,
					Dir: uint8(rng.Intn(16)), Kind: uint8(1 + rng.Intn(4))})
			}
		}
		out = append(out, s)
		if rng.Intn(30) == 0 { // a duplicate (id, t) with different content
			d := s
			d.X += 3
			out = append(out, d)
		}
	}
	return out
}

func sameWindow(t *testing.T, what string, got, want *core.MoverWindow) {
	t.Helper()
	norm := func(w *core.MoverWindow) *core.MoverWindow {
		c := memstore.CloneWindow(w)
		for i := range c.Rows {
			if len(c.Rows[i].Carriages) == 0 {
				c.Rows[i].Carriages = nil
			}
			if len(c.Rows[i].Rail) == 0 {
				c.Rows[i].Rail = nil
			}
		}
		if len(c.Movers) == 0 {
			c.Movers = nil
		}
		if len(c.Rows) == 0 {
			c.Rows = nil
		}
		return c
	}
	g, w := norm(got), norm(want)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s differs:\n got  %+v\n want %+v", what, g, w)
	}
}

// compareStores checks every read of a against the reference b.
func compareStores(t *testing.T, ctx context.Context, a, b core.MoverStore, t0, t1 core.TimeMs) {
	t.Helper()
	sa, err := a.Surfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := b.Surfaces(ctx)
	if !reflect.DeepEqual(sa, sb) {
		t.Fatalf("surfaces:\n got  %+v\n want %+v", sa, sb)
	}
	for _, info := range sb {
		for _, day := range info.Days {
			da, err := a.Day(ctx, info.Key, day)
			if err != nil {
				t.Fatalf("day %s: %v", day, err)
			}
			db, _ := b.Day(ctx, info.Key, day)
			if !reflect.DeepEqual(da, db) {
				t.Fatalf("day %s:\n got  %+v\n want %+v", day, da, db)
			}
			for _, ref := range db.Windows {
				wa, err := a.Window(ctx, info.Key, ref.Start)
				if err != nil {
					t.Fatalf("window %d: %v", ref.Start, err)
				}
				wb, _ := b.Window(ctx, info.Key, ref.Start)
				sameWindow(t, fmt.Sprintf("window %v %d", info.Key, ref.Start), wa, wb)
			}
			for _, k := range db.Keyframes {
				ka, err := a.Keyframe(ctx, info.Key, k)
				if err != nil {
					t.Fatalf("keyframe %d: %v", k, err)
				}
				kb, _ := b.Keyframe(ctx, info.Key, k)
				sameWindow(t, fmt.Sprintf("keyframe %v %d", info.Key, k), ka, kb)
			}
		}
		// Live: every 37 s around the horizon (memory path and its edge), and a few
		// historical instants (disk path).
		var nows []core.TimeMs
		edge := max(info.Horizon, t0) - 3*core.Minute
		for now := edge; now <= t1+core.Minute; now += 37 * core.Second {
			nows = append(nows, now)
		}
		for now := t0 - core.Minute; now < edge; now += 23*core.Minute + 7*core.Second {
			nows = append(nows, now)
		}
		for _, now := range nows {
			la, err := a.Live(ctx, info.Key, now)
			if err != nil {
				t.Fatalf("live %d: %v", now, err)
			}
			lb, _ := b.Live(ctx, info.Key, now)
			sameWindow(t, fmt.Sprintf("live %v at %d (horizon %d)", info.Key, now, info.Horizon), la, lb)
		}
		// A window that is still open and a misaligned start are not served.
		if _, err := a.Window(ctx, info.Key, info.Horizon); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("open window: %v", err)
		}
		if _, err := a.Window(ctx, info.Key, info.Horizon-core.MoverWindowLen+1); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("misaligned window: %v", err)
		}
	}
}

// TestDifferential feeds the same random batches to the store and to memstore, with
// commits in between, reopening the store at random points, and compares every read.
func TestDifferential(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			opts := Options{SyncInterval: -1, MaxAhead: -1, MaxBehind: -1} // memstore has no clock bounds
			a, err := Open(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.Close() }()
			b := memstore.NewMovers()
			t0 := coretest.T0 - 23*core.Hour // crosses a UTC midnight
			now := t0
			for step := 0; step < 30; step++ {
				// Samples spread around the current clock, some late.
				batch := randomSamples(rng, now-2*core.Minute, 9*core.Minute, 40+rng.Intn(120))
				ra, err := a.Append(ctx, batch)
				if err != nil {
					t.Fatal(err)
				}
				rb, _ := b.Append(ctx, batch)
				if ra != rb {
					t.Fatalf("step %d append: got %+v want %+v", step, ra, rb)
				}
				now += core.TimeMs(rng.Int63n(int64(40 * core.Minute)))
				if rng.Intn(3) > 0 {
					upTo := now - core.TimeMs(rng.Int63n(int64(core.Minute)))
					if err := a.Commit(ctx, upTo); err != nil {
						t.Fatal(err)
					}
					b.Commit(ctx, upTo)
				}
				if rng.Intn(4) == 0 {
					if err := a.Close(); err != nil {
						t.Fatal(err)
					}
					if a, err = Open(dir, opts); err != nil {
						t.Fatal(err)
					}
				}
				compareStores(t, ctx, a, b, t0, now)
			}
		})
	}
}

// TestWALReplay appends without committing, drops the store without Close (a crash
// after the last fsync) and checks that reopening recovers every open sample.
func TestWALReplay(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	dir := t.TempDir()
	a, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b := memstore.NewMovers()
	t0 := coretest.T0
	batch := randomSamples(rng, t0, 20*core.Minute, 500)
	a.Append(ctx, batch[:200])
	b.Append(ctx, batch[:200])
	a.Commit(ctx, t0+10*core.Minute)
	b.Commit(ctx, t0+10*core.Minute)
	a.Append(ctx, batch[200:])
	b.Append(ctx, batch[200:])
	// Crash: the files stay as they are (fsynced per batch); nothing is closed.
	a.wal.f.Close()
	c, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	compareStores(t, ctx, c, b, t0, t0+20*core.Minute)
	if err := c.Commit(ctx, t0+30*core.Minute); err != nil {
		t.Fatal(err)
	}
	b.Commit(ctx, t0+30*core.Minute)
	compareStores(t, ctx, c, b, t0, t0+30*core.Minute)
}

// TestWALTornTail cuts the WAL in the middle of the last frame and flips a byte in a
// copy: both reopen with the complete frames before the damage.
func TestWALTornTail(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(8))
	for _, mode := range []string{"truncate", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			a, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			b := memstore.NewMovers()
			first := randomSamples(rng, coretest.T0, 4*core.Minute, 100)
			second := randomSamples(rng, coretest.T0, 4*core.Minute, 100)
			a.Append(ctx, first)
			b.Append(ctx, first)
			path := filepath.Join(dir, "movers", "wal", "wal.log")
			st, _ := os.Stat(path)
			a.Append(ctx, second)
			a.wal.f.Close()
			raw, _ := os.ReadFile(path)
			if mode == "truncate" {
				raw = raw[:st.Size()+(int64(len(raw))-st.Size())/2]
			} else {
				raw[st.Size()+20] ^= 0xFF
			}
			os.WriteFile(path, raw, 0o644)
			c, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if got := c.Stats().OpenRows; got != len(first) {
				t.Fatalf("open rows after recovery = %d, want %d", got, len(first))
			}
			if st2, _ := os.Stat(path); st2.Size() != st.Size() {
				t.Fatalf("torn tail not truncated: %d, want %d", st2.Size(), st.Size())
			}
			// Appending after recovery and reopening again still works.
			third := randomSamples(rng, coretest.T0, 4*core.Minute, 50)
			c.Append(ctx, third)
			b.Append(ctx, third)
			c.Close()
			d, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ok(t, d.Commit(ctx, coretest.T0+core.Hour))
			ok(t, b.Commit(ctx, coretest.T0+core.Hour))
			compareStores(t, ctx, d, b, coretest.T0, coretest.T0+core.Hour)
		})
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOverloaded(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1, MaxOpenRows: 100})
	defer m.Close()
	rng := rand.New(rand.NewSource(9))
	batch := randomSamples(rng, coretest.T0, 4*core.Minute, 80)[:80]
	if _, err := m.Append(ctx, batch); err != nil {
		t.Fatal(err)
	}
	res, err := m.Append(ctx, batch[:30])
	if !errors.Is(err, ErrOverloaded) || res != (core.ApplyResult{}) {
		t.Fatalf("append over the limit = %+v, %v", res, err)
	}
	ok(t, m.Commit(ctx, coretest.T0+core.Hour))
	if st := m.Stats(); st.OpenRows != 0 {
		t.Fatalf("open rows after commit = %d", st.OpenRows)
	}
	if _, err := m.Append(ctx, batch[:30]); err != nil {
		t.Fatalf("append after commit: %v", err)
	}
}

func TestInvalidCoordinates(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1})
	defer m.Close()
	s := core.MoverSample{Surface: coretest.SFC, Meta: coretest.Train, T: coretest.T0, X: 1e300}
	res, err := m.Append(ctx, []core.MoverSample{s})
	if err != nil || res.Invalid != 1 {
		t.Fatalf("huge coordinate = %+v, %v", res, err)
	}
	if _, err := m.Live(ctx, coretest.SFC, coretest.T0); !errors.Is(err, core.ErrUnknownDatastore) {
		t.Fatalf("invalid sample created a surface: %v", err)
	}
}

// TestLiveBounded checks that the memory kept for Live stays bounded on a long run:
// the trail holds about LiveTrail of rows and the live map one row per live journey.
func TestLiveBounded(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1, MaxTrailRows: 500, MaxLiveMovers: 50})
	defer m.Close()
	t0 := coretest.T0
	// 200 journeys, 2 samples/s each for 30 minutes, committed every minute.
	for sec := core.TimeMs(0); sec < 30*core.Minute; sec += core.Second {
		var batch []core.MoverSample
		for j := 0; j < 200; j++ {
			batch = append(batch, core.MoverSample{Surface: coretest.SFC, Meta: core.MoverMeta{ID: fmt.Sprint("j", j), Kind: core.KindVehicle, Force: "player", Name: "car"},
				T: t0 + sec + core.TimeMs(j), X: float64(sec / 100), Y: float64(j)})
		}
		if _, err := m.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if sec%core.Minute == 0 {
			ok(t, m.Commit(ctx, t0+sec))
		}
		st := m.Stats()
		if st.TrailRows > 500 || st.LiveMovers > 50 || st.OpenRows > 200*(6*60+1) {
			t.Fatalf("at %v: %+v", sec, st)
		}
	}
	st := m.Stats()
	if st.Evicted == 0 || st.LiveMovers != 50 {
		t.Fatalf("live map not bounded: %+v", st)
	}
	l, err := m.Live(ctx, coretest.SFC, t0+30*core.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Movers) != 200 {
		t.Fatalf("live movers = %d", len(l.Movers))
	}
}

// TestLiveBoundedAcrossSurfaces checks that MaxTrailRows and MaxLiveMovers bound the
// totals over all surfaces, also right after a reopen, and that every surface keeps at
// least an equal share.
func TestLiveBoundedAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	opts := Options{SyncInterval: -1, MaxTrailRows: 500, MaxLiveMovers: 50}
	m, dir := openTemp(t, opts)
	t0 := coretest.T0
	const surfaces = 4
	check := func(when string, m *Store) {
		t.Helper()
		st := m.Stats()
		if st.TrailRows > opts.MaxTrailRows || st.LiveMovers > opts.MaxLiveMovers {
			t.Fatalf("%s: totals over the limits: %+v", when, st)
		}
		for i := 0; i < surfaces; i++ {
			s := m.sfc[core.SurfaceKey{Map: "m", Surface: fmt.Sprint("s", i)}]
			if s == nil || len(s.live) < opts.MaxLiveMovers/surfaces || len(s.trail) < opts.MaxTrailRows/surfaces {
				t.Fatalf("%s: surface %d lost its share", when, i)
			}
		}
	}
	for sec := core.TimeMs(0); sec <= 20*core.Minute; sec += core.Second {
		var batch []core.MoverSample
		for i := 0; i < surfaces; i++ {
			for j := 0; j < 60; j++ {
				batch = append(batch, core.MoverSample{Surface: core.SurfaceKey{Map: "m", Surface: fmt.Sprint("s", i)},
					Meta: core.MoverMeta{ID: fmt.Sprint("j", i, "-", j), Kind: core.KindVehicle, Force: "player", Name: "car"},
					T:    t0 + sec + core.TimeMs(j), X: float64(sec / 100), Y: float64(j)})
			}
		}
		if _, err := m.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if sec%core.Minute == 0 && sec > 0 {
			ok(t, m.Commit(ctx, t0+sec))
			if sec%core.MoverWindowLen == 0 { // a window closed
				check(fmt.Sprint("after commit at ", sec), m)
			}
		}
	}
	ok(t, m.Close())
	// Open rebuilds each surface's trail and live map from disk with the whole limits
	// and must then trim them to their shares.
	m, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	check("after reopen", m)
}

// TestConcurrent runs appends, commits and every read at the same time (the race
// detector needs cgo, which this repository's Windows CI does not have; this at least
// catches deadlocks, panics and inconsistent reads).
func TestConcurrent(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: 5 * time.Millisecond})
	defer m.Close()
	t0 := coretest.T0
	var clock atomic.Int64
	clock.Store(int64(t0))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	wg.Add(1)
	go func() { // writer: 50 trains at 5 Hz of map time, 10 s of map time per batch
		defer wg.Done()
		for now := t0; now < t0+3*core.Hour; now += 10 * core.Second {
			var batch []core.MoverSample
			for k := core.TimeMs(0); k < 10*core.Second; k += 200 {
				batch = append(batch, trainBatch(50, now+k)...)
			}
			if _, err := m.Append(ctx, batch); err != nil {
				errs <- err
				return
			}
			clock.Store(int64(now))
			if err := m.Commit(ctx, now-15*core.Second); err != nil {
				errs <- err
				return
			}
		}
		close(stop)
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := core.TimeMs(clock.Load())
				l, err := m.Live(ctx, coretest.SFC, now-core.TimeMs(r)*core.Minute)
				if err != nil && !errors.Is(err, core.ErrUnknownDatastore) {
					errs <- err
					return
				}
				if l != nil && len(l.Rows) > 0 && len(l.Movers) == 0 {
					errs <- fmt.Errorf("live rows without movers")
					return
				}
				for _, info := range must(m.Surfaces(ctx)) {
					for _, d := range info.Days {
						day, err := m.Day(ctx, info.Key, d)
						if err != nil {
							errs <- err
							return
						}
						if len(day.Windows) > 0 {
							ref := day.Windows[len(day.Windows)-1]
							w, err := m.Window(ctx, info.Key, ref.Start)
							if err != nil {
								errs <- fmt.Errorf("window %d: %v", ref.Start, err)
								return
							}
							if len(w.Rows) != ref.Rows {
								errs <- fmt.Errorf("window %d: %d rows, index says %d", ref.Start, len(w.Rows), ref.Rows)
								return
							}
						}
					}
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
