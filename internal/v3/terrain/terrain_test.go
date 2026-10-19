package terrain

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
)

func openTemp(t testing.TB, opt Options) *Store {
	t.Helper()
	if opt.SyncInterval == 0 {
		opt.NoSync = true
	}
	s, err := Open(t.TempDir(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	coretest.TerrainStore(t, func(t *testing.T) core.TerrainStore { return openTemp(t, Options{}) })
}

func TestDifferential(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore { return openTemp(t, Options{}) },
			coretest.DiffOptions{Seed: seed, Updates: 4000, ExactVersions: true})
	}
}

// Small keyframe settings put many keyframes and long resumable chains in the test.
func TestDifferentialKeyframes(t *testing.T) {
	coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore {
		return openTemp(t, Options{MaxChain: 3, KeyframeFactor: 0.5, DecodedCacheBytes: 3 * imageBytes})
	}, coretest.DiffOptions{Seed: 9, Updates: 4000, ExactVersions: true})
}

// reopening closes and reopens the store every few calls, so the differential test
// also covers recovery: the WAL replay of open windows and the tile summaries.
type reopening struct {
	*Store
	t     *testing.T
	dir   string
	opt   Options
	calls int
	every int
}

func (r *reopening) maybeReopen() {
	r.calls++
	if r.calls%r.every != 0 {
		return
	}
	if err := r.Store.Close(); err != nil {
		r.t.Fatal(err)
	}
	s, err := Open(r.dir, r.opt)
	if err != nil {
		r.t.Fatal(err)
	}
	r.Store = s
}

func (r *reopening) Apply(ctx context.Context, u []core.ChunkUpdate) (core.ApplyResult, error) {
	r.maybeReopen()
	return r.Store.Apply(ctx, u)
}

func (r *reopening) Commit(ctx context.Context, upTo core.TimeMs) error {
	r.maybeReopen()
	return r.Store.Commit(ctx, upTo)
}

func (r *reopening) Close() error { return r.Store.Close() }

func TestDifferentialReopen(t *testing.T) {
	coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore {
		dir := t.TempDir()
		opt := Options{NoSync: true, MaxChain: 4}
		s, err := Open(dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		return &reopening{Store: s, t: t, dir: dir, opt: opt, every: 97}
	}, coretest.DiffOptions{Seed: 5, Updates: 3000, ExactVersions: true})
}

func TestCodecRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pal := newLUT()
	var colors []uint16
	add := func(c uint16) {
		if pal[c] == 0 {
			colors = append(colors, c)
			pal[c] = uint32(len(colors))
		}
	}
	add(0)
	for len(colors) < 700 { // wide palette
		add(uint16(r.Intn(1 << 16)))
	}
	for _, wideColours := range []bool{false, true} {
		n := 200
		if wideColours {
			n = len(colors)
		}
		old := core.NewTileImage()
		for i := range old.Pix {
			old.Pix[i] = colors[r.Intn(n)]
		}
		var changes []chunkChange
		want := old.Clone()
		var scratch [core.ChunkPixelCount]uint16
		for b := 0; b < core.TileBlocks; b += 1 + r.Intn(3) {
			px := old.Block(uint8(b))
			switch r.Intn(4) {
			case 0: // one pixel
				px[r.Intn(1024)] = colors[r.Intn(n)]
			case 1: // up to 128
				for k := r.Intn(128); k >= 0; k-- {
					px[r.Intn(1024)] = colors[r.Intn(n)]
				}
			case 2: // bitmap range
				for k := 129 + r.Intn(380); k >= 0; k-- {
					px[r.Intn(1024)] = colors[r.Intn(n)]
				}
			default: // whole chunk
				for i := range px {
					px[i] = colors[r.Intn(n)]
				}
			}
			if c, ok := diffChunk(uint8(b), old, &px, pal, scratch[:]); ok {
				changes = append(changes, c)
				want.SetBlock(uint8(b), &px)
			}
		}
		wide := maxIndex(changes) > 255
		if wide != wideColours {
			t.Fatalf("wide = %v", wide)
		}
		raw := encodeDelta(nil, changes, wide)
		got := old.Clone()
		if err := applyDelta(got, raw, wide, colors, scratch[:]); err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Fatal("delta round trip differs")
		}
		kf, kwide := encodeKeyframe(nil, want, pal)
		if kwide != wideColours {
			t.Fatalf("keyframe wide = %v", kwide)
		}
		back := core.NewTileImage()
		if err := decodeKeyframe(back, kf, kwide, colors); err != nil {
			t.Fatal(err)
		}
		if !back.Equal(want) {
			t.Fatal("keyframe round trip differs")
		}
		// Truncated payloads are errors, never panics.
		for cut := 0; cut < len(raw); cut += 1 + len(raw)/50 {
			_ = applyDelta(old.Clone(), raw[:cut], wide, colors, scratch[:])
		}
	}
}

// TestWidePalette stores more than 256 colours through the store.
func TestWidePalette(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{})
	defer s.Close()
	ds := coretest.DS
	var ups []core.ChunkUpdate
	c := uint16(1)
	for i := 0; i < 2; i++ {
		px := &core.ChunkPixels{}
		for j := range px {
			px[j] = c
			c++
		}
		ups = append(ups, core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(i), Y: 0}, T: 1000, Pixels: px})
	}
	if _, err := s.Apply(ctx, ups); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, core.Minute); err != nil {
		t.Fatal(err)
	}
	pal, _ := s.Palette(ctx, ds)
	if pal.Version() != 2049 {
		t.Fatalf("palette version %d", pal.Version())
	}
	s.DropCaches()
	tile, b0 := ups[0].Chunk.Tile()
	im, err := s.ReadVersion(ctx, ds, tile, 1)
	if err != nil {
		t.Fatal(err)
	}
	if im.Block(b0) != *ups[0].Pixels {
		t.Fatal("wide content differs")
	}
}

func TestNoAllocForUnknownTiles(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{})
	defer s.Close()
	ds := coretest.DS
	s.Apply(ctx, []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{}, T: 1, Pixels: coretest.Fill(3)}})
	s.Commit(ctx, core.Minute)
	other := core.DatastoreKey{Map: "x", Surface: "y", Force: "z"}
	r := rand.New(rand.NewSource(1))
	before := s.Stats()
	allocs := testing.AllocsPerRun(2000, func() {
		tc := core.TileCoord{X: int32(r.Intn(4000) - 2000), Y: int32(r.Intn(4000) - 2000)}
		if tc == (core.TileCoord{}) {
			return
		}
		if _, err := s.VersionAt(ctx, ds, tc, 5); !errors.Is(err, core.ErrNoTile) {
			t.Fatal(err)
		}
		if _, _, err := s.ReadTile(ctx, ds, core.TileCoord{X: 1 << 20}, 5, true); !errors.Is(err, core.ErrBadTile) {
			t.Fatal(err)
		}
		if _, err := s.ReadVersion(ctx, other, tc, 1); !errors.Is(err, core.ErrUnknownDatastore) {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("%.1f allocations per unknown-tile request", allocs)
	}
	after := s.Stats()
	if after.Tiles != before.Tiles || after.Series.Open != before.Series.Open {
		t.Fatalf("unknown tiles created state: %+v -> %+v", before, after)
	}
}

func TestOverBudget(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{OpenWindowBytes: 10 * openChunkBytes})
	defer s.Close()
	ds := coretest.DS
	var ups []core.ChunkUpdate
	for i := 0; i < 12; i++ {
		ups = append(ups, core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(i)}, T: 10, Pixels: coretest.Fill(uint16(i + 1))})
	}
	if _, err := s.Apply(ctx, ups); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, ups[:1]); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	if err := s.Commit(ctx, core.Minute); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.OpenWindowBytes != 0 {
		t.Fatalf("open bytes after commit %d", st.OpenWindowBytes)
	}
	if _, err := s.Apply(ctx, []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{}, T: core.Minute + 1, Pixels: coretest.Fill(9)}}); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentReadsDuringIngest runs readers against a store that is ingesting and
// committing, and checks every committed read against the oracle afterwards (run with
// -race to check the locking).
func TestConcurrentReadsDuringIngest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{MaxChain: 5, DecodedCacheBytes: 8 * imageBytes})
	defer s.Close()
	ds := coretest.DS
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				tc := core.TileCoord{X: int32(r.Intn(3) - 1), Y: int32(r.Intn(3) - 1)}
				_, _, err := s.ReadTile(ctx, ds, tc, core.TimeMs(r.Int63n(int64(200*core.Minute))), r.Intn(2) == 0)
				if err != nil && !errors.Is(err, core.ErrNoTile) && !errors.Is(err, core.ErrUnknownDatastore) {
					t.Error(err)
					return
				}
				s.Datastores(ctx)
				s.ListTiles(ctx, ds, 0)
			}
		}(g)
	}
	r := rand.New(rand.NewSource(42))
	var now core.TimeMs
	for i := 0; i < 3000; i++ {
		now += core.TimeMs(r.Intn(5000))
		px := coretest.Fill(uint16(r.Intn(5)))
		if _, err := s.Apply(ctx, []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{X: int32(r.Intn(48) - 24), Y: int32(r.Intn(48) - 24)}, T: now, Pixels: px}}); err != nil {
			t.Fatal(err)
		}
		if i%40 == 0 {
			if err := s.Commit(ctx, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
}
