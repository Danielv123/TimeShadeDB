package movers

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
	"timeshadedb/internal/core/memstore"
)

// railJourneys makes trains of several lengths driving along random tracks (stops included),
// a sample every 4 s from t0, and returns their samples and the body each sample's row must
// carry in a keyframe or as the first row of the live payload.
func railJourneys(rng *rand.Rand, t0 core.TimeMs, n int) ([]core.MoverSample, map[string]map[core.TimeMs][]core.RailPiece) {
	var all []core.MoverSample
	bodies := map[string]map[core.TimeMs][]core.RailPiece{}
	for j, cars := range []int{2, 9, 40, 108} {
		meta := coretest.Train
		meta.ID = fmt.Sprintf("1.0.%d", j)
		meta.Carriages = cars
		tr := coretest.NewRailTrack(rng, float64(n)*30+float64(cars*7), 2)
		run := tr.Run(rng, coretest.SFC, meta, t0, float64(cars*7), 3, n, 25+j*10)
		bodies[meta.ID] = map[core.TimeMs][]core.RailPiece{}
		for i := range run.Samples {
			s := &run.Samples[i]
			s.T = t0 + core.TimeMs(i)*4000 + core.TimeMs(j)*7
			s.Tick = int64(s.T-t0) * 60 / 1000
			bodies[meta.ID][s.T] = run.Bodies[i]
		}
		all = append(all, run.Samples...)
	}
	return all, bodies
}

// checkBodies checks that each journey's first row in w (a keyframe or live payload) carries
// its body in front of its own pieces.
func checkBodies(t *testing.T, what string, w *core.MoverWindow, samples map[string]map[core.TimeMs]core.MoverSample, bodies map[string]map[core.TimeMs][]core.RailPiece) int {
	t.Helper()
	seen := map[string]bool{}
	with := 0
	for _, r := range w.Rows {
		id := w.Movers[r.Mover].ID
		if seen[id] {
			continue
		}
		seen[id] = true
		s, ok := samples[id][r.T]
		if !ok {
			t.Fatalf("%s: row %s at %d is no sample", what, id, r.T)
		}
		want := core.RailWithBody(bodies[id][r.T], s.Quantized().Rail)
		if len(r.Rail) != len(want) || (len(want) > 0 && !reflect.DeepEqual(r.Rail, want)) {
			t.Fatalf("%s: %s at %d rail\n got  %v\n want %v (body %v)", what, id, r.T, r.Rail, want, bodies[id][r.T])
		}
		if len(bodies[id][r.T]) > 0 {
			with++
		}
	}
	return with
}

// TestRailBodies feeds trains on random tracks to the store and to memstore in batches
// that arrive partly out of order, commits behind the clock, reopens the store now and
// then, and checks that keyframes and live payloads carry the bodies (and equal memstore's
// reads everywhere).
func TestRailBodies(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			t0 := coretest.T0 - 20*core.Minute
			all, bodies := railJourneys(rng, t0, 2400) // 2 h 40 min
			samples := map[string]map[core.TimeMs]core.MoverSample{}
			for _, s := range all {
				if samples[s.Meta.ID] == nil {
					samples[s.Meta.ID] = map[core.TimeMs]core.MoverSample{}
				}
				samples[s.Meta.ID][s.T] = s
			}
			sort.Slice(all, func(i, j int) bool { return all[i].T < all[j].T })
			dir := t.TempDir()
			opts := Options{SyncInterval: -1, MaxAhead: -1, MinTime: -1}
			a, err := Open(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.Close() }()
			b := memstore.NewMovers()
			keyframes, livesWith := 0, 0
			for i := 0; i < len(all); {
				n := min(len(all)-i, 20+rng.Intn(60))
				batch := append([]core.MoverSample(nil), all[i:i+n]...)
				if rng.Intn(3) == 0 {
					// some rows of the batch arrive out of time order (a retry), some twice
					rng.Shuffle(len(batch), func(x, y int) { batch[x], batch[y] = batch[y], batch[x] })
					batch = append(batch, batch[rng.Intn(len(batch))])
				}
				i += n
				ra, err := a.Append(ctx, batch)
				if err != nil {
					t.Fatal(err)
				}
				if rb, _ := b.Append(ctx, batch); ra != rb {
					t.Fatalf("append: %+v, memstore %+v", ra, rb)
				}
				now := batch[0].T
				for _, s := range batch {
					now = max(now, s.T)
				}
				if rng.Intn(4) == 0 {
					upTo := now - 15*core.Second
					if err := a.Commit(ctx, upTo); err != nil {
						t.Fatal(err)
					}
					b.Commit(ctx, upTo)
				}
				if rng.Intn(40) == 0 {
					if err := a.Close(); err != nil {
						t.Fatal(err)
					}
					if a, err = Open(dir, opts); err != nil {
						t.Fatal(err)
					}
				}
				if rng.Intn(10) == 0 {
					l, err := a.Live(ctx, coretest.SFC, now)
					if err != nil {
						t.Fatal(err)
					}
					livesWith += checkBodies(t, fmt.Sprintf("live at %d", now), l, samples, bodies)
					lb, _ := b.Live(ctx, coretest.SFC, now)
					sameWindow(t, fmt.Sprintf("live at %d", now), l, lb)
				}
			}
			end := all[len(all)-1].T + 10*core.Minute
			if err := a.Commit(ctx, end); err != nil {
				t.Fatal(err)
			}
			b.Commit(ctx, end)
			compareStores(t, ctx, a, b, t0, end)
			infos, _ := a.Surfaces(ctx)
			for _, info := range infos {
				for _, day := range info.Days {
					d, _ := a.Day(ctx, info.Key, day)
					for _, k := range d.Keyframes {
						w, err := a.Keyframe(ctx, info.Key, k)
						if err != nil {
							t.Fatal(err)
						}
						if checkBodies(t, fmt.Sprintf("keyframe %d", k), w, samples, bodies) > 0 {
							keyframes++
						}
					}
				}
				// the live payload rebuilt from disk at past instants
				for now := t0 + 3*core.Minute; now < info.Horizon; now += 17*core.Minute + 3*core.Second {
					l, err := a.Live(ctx, info.Key, now)
					if err != nil {
						t.Fatal(err)
					}
					livesWith += checkBodies(t, fmt.Sprintf("live from disk at %d", now), l, samples, bodies)
				}
			}
			if keyframes < 2 || livesWith == 0 {
				t.Fatalf("bodies seen in %d keyframes and %d live rows", keyframes, livesWith)
			}
		})
	}
}

// benchRail appends trains on random tracks (railJourneys, rail path rows; with plain, the
// same rows without the rail fields) in time order in batches of 200, committing every 10 s
// of data, to a fresh store without a WAL per iteration.
func benchRail(b *testing.B, plain bool) {
	ctx := context.Background()
	all, _ := railJourneys(rand.New(rand.NewSource(1)), coretest.T0, 4000)
	sort.Slice(all, func(i, j int) bool { return all[i].T < all[j].T })
	if plain {
		for i := range all {
			all[i].Flags &^= core.FlagRail
			all[i].Rail, all[i].Speed, all[i].RailStart = nil, 0, false
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		m, err := Open(b.TempDir(), Options{SyncInterval: -1, NoWAL: true, MaxAhead: -1, MinTime: -1})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		next := all[0].T + 10*core.Second
		for k := 0; k < len(all); k += 200 {
			batch := all[k:min(k+200, len(all))]
			if _, err := m.Append(ctx, batch); err != nil {
				b.Fatal(err)
			}
			if t := batch[len(batch)-1].T; t >= next {
				if err := m.Commit(ctx, t-15*core.Second); err != nil {
					b.Fatal(err)
				}
				next = t + 10*core.Second
			}
		}
		b.StopTimer()
		m.Close()
		b.StartTimer()
	}
	b.ReportMetric(float64(len(all)*b.N)/b.Elapsed().Seconds(), "samples/s")
}

// BenchmarkAppendRail: 4 trains (2 to 108 carriages) of rail path rows with pieces, 16,000
// samples (4.4 h of data), bodies followed as they arrive and at every commit.
func BenchmarkAppendRail(b *testing.B) { benchRail(b, false) }

// BenchmarkAppendRailPlain: the same rows without the rail fields.
func BenchmarkAppendRailPlain(b *testing.B) { benchRail(b, true) }

// TestRailBodiesReopenAtKeyframe reopens the store just after it wrote a keyframe, so the
// live trail reaches back before the keyframe it rebuilds from, and checks that the first
// rows from before the keyframe still carry their bodies (and equal memstore's).
func TestRailBodiesReopenAtKeyframe(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	t0 := coretest.T0 - 20*core.Minute
	all, bodies := railJourneys(rng, t0, 600) // to T0 + 20 min
	samples := map[string]map[core.TimeMs]core.MoverSample{}
	var upTo []core.MoverSample
	for _, s := range all {
		if samples[s.Meta.ID] == nil {
			samples[s.Meta.ID] = map[core.TimeMs]core.MoverSample{}
		}
		samples[s.Meta.ID][s.T] = s
		if s.T <= coretest.T0+30*core.Second {
			upTo = append(upTo, s)
		}
	}
	sort.Slice(upTo, func(i, j int) bool { return upTo[i].T < upTo[j].T })
	dir := t.TempDir()
	opts := Options{SyncInterval: -1, MaxAhead: -1, MinTime: -1}
	a, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	b := memstore.NewMovers()
	if _, err := a.Append(ctx, upTo); err != nil {
		t.Fatal(err)
	}
	b.Append(ctx, upTo)
	if err := a.Commit(ctx, coretest.T0); err != nil {
		t.Fatal(err)
	}
	b.Commit(ctx, coretest.T0)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a, err = Open(dir, opts); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	with := 0
	for now := coretest.T0 + 5*core.Second; now <= coretest.T0+30*core.Second; now += 5 * core.Second {
		l, err := a.Live(ctx, coretest.SFC, now)
		if err != nil {
			t.Fatal(err)
		}
		with += checkBodies(t, fmt.Sprintf("live at %d", now), l, samples, bodies)
		lb, _ := b.Live(ctx, coretest.SFC, now)
		sameWindow(t, fmt.Sprintf("live at %d", now), l, lb)
	}
	if with == 0 {
		t.Fatal("no live row with a body")
	}
}

// TestLiveHeldEvicting holds the horizon (no commit) over an hour of trains on random
// tracks with an open-window budget so small that every window but the newest is evicted,
// and checks the live payload at instants across the open hour: it reads back at most the
// windows its trail overlaps, carries the bodies, and equals memstore's.
func TestLiveHeldEvicting(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))
	t0 := coretest.T0 - 2*core.Hour
	all, bodies := railJourneys(rng, t0, 900) // 1 h
	samples := map[string]map[core.TimeMs]core.MoverSample{}
	for _, s := range all {
		if samples[s.Meta.ID] == nil {
			samples[s.Meta.ID] = map[core.TimeMs]core.MoverSample{}
		}
		samples[s.Meta.ID][s.T] = s
	}
	sort.Slice(all, func(i, j int) bool { return all[i].T < all[j].T })
	a, _ := openTemp(t, Options{SyncInterval: -1, MaxAhead: -1, MinTime: -1, OpenBytes: 16 << 10})
	defer a.Close()
	b := memstore.NewMovers()
	for i := 0; i < len(all); i += 100 {
		batch := all[i:min(i+100, len(all))]
		if _, err := a.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
		b.Append(ctx, batch)
	}
	if st := a.Stats(); st.SpilledWindows < 10 {
		t.Fatalf("%d windows evicted, want most of 12", st.SpilledWindows)
	}
	with := 0
	end := all[len(all)-1].T
	for now := t0 + 30*core.Second; now <= end; now += 7*core.Minute + 13*core.Second {
		before := a.Stats().LiveReadBacks
		l, err := a.Live(ctx, coretest.SFC, now)
		if err != nil {
			t.Fatal(err)
		}
		if n := a.Stats().LiveReadBacks - before; n > 2 {
			t.Fatalf("live at %d read back %d windows, want at most the 2 its trail overlaps", now, n)
		}
		with += checkBodies(t, fmt.Sprintf("live at %d", now), l, samples, bodies)
		lb, _ := b.Live(ctx, coretest.SFC, now)
		sameWindow(t, fmt.Sprintf("live at %d", now), l, lb)
	}
	if with == 0 {
		t.Fatal("no live row with a body")
	}
}
