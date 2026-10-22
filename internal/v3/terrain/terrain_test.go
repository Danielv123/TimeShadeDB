package terrain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
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

// TestSpillInsteadOfRefusing: past the open-window budget Apply spills the oldest
// windows to disk instead of refusing; live reads still see the spilled chunks, and the
// commit closes the spilled windows and deletes their files.
func TestSpillInsteadOfRefusing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true, OpenWindowBytes: 10 * openChunkBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := memstore.NewTerrain()
	ds := coretest.DS
	// Three windows of 8 chunks each, then a second update of window 0 after it spilled.
	var ups []core.ChunkUpdate
	for w := 0; w < 3; w++ {
		for i := 0; i < 8; i++ {
			ups = append(ups, core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(i)}, T: core.TimeMs(w)*core.Minute + 10, Pixels: coretest.Fill(uint16(w*8 + i + 1))})
		}
	}
	for i := 0; i < len(ups); i += 4 {
		if res, err := s.Apply(ctx, ups[i:i+4]); err != nil || res.Accepted != 4 {
			t.Fatalf("apply %d = %+v, %v", i, res, err)
		}
		want.Apply(ctx, ups[i:i+4])
	}
	late := []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{X: 1}, T: 20, Pixels: coretest.Fill(99)}, {DS: ds, Chunk: core.ChunkCoord{X: 2}, T: 5, Pixels: coretest.Fill(98)}}
	if _, err := s.Apply(ctx, late); err != nil {
		t.Fatal(err)
	}
	want.Apply(ctx, late)
	st := s.Stats()
	if st.Spills == 0 || st.SpilledWindows == 0 || st.OpenWindowBytes > 10*openChunkBytes || st.Accepted != uint64(len(ups)+2) {
		t.Fatalf("stats = %+v", st)
	}
	tile := core.TileCoord{}
	for _, at := range []core.TimeMs{3 * core.Minute, 30} {
		a, sa, err := s.ReadTile(ctx, ds, tile, at, true)
		if err != nil {
			t.Fatal(err)
		}
		b, sb, _ := want.ReadTile(ctx, ds, tile, at, true)
		if !a.Equal(b) || sa.Live != sb.Live {
			t.Fatalf("live read at %d differs (live %v, oracle %v)", at, sa.Live, sb.Live)
		}
	}
	far := 10 * core.Minute
	ok := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	ok(s.Commit(ctx, far))
	want.Commit(ctx, far)
	if st := s.Stats(); st.OpenWindowBytes != 0 || st.SpilledWindows != 0 {
		t.Fatalf("after commit: %+v", st)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "datastores", "*", "open", "*.spill")); len(files) != 0 {
		t.Fatalf("spill files left: %v", files)
	}
	for v := core.Version(0); v <= 3; v++ {
		a, err := s.ReadVersion(ctx, ds, tile, v)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := want.ReadVersion(ctx, ds, tile, v)
		if !a.Equal(b) {
			t.Fatalf("version %d differs", v)
		}
	}
}

// TestDifferentialSpilling is TestDifferential with an open-window budget so small that
// almost every window is spilled before its commit.
func TestDifferentialSpilling(t *testing.T) {
	for _, seed := range []int64{1, 2} {
		coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore {
			return openTemp(t, Options{OpenWindowBytes: 24 * openChunkBytes})
		}, coretest.DiffOptions{Seed: seed, Updates: 4000, ExactVersions: true})
	}
}

// TestDifferentialReopenSpilling reopens a spilling store every few calls: Open deletes
// the spill files and the WAL replay spills again.
func TestDifferentialReopenSpilling(t *testing.T) {
	coretest.TerrainDifferential(t, func(t *testing.T) core.TerrainStore {
		dir := t.TempDir()
		opt := Options{NoSync: true, MaxChain: 4, OpenWindowBytes: 16 * openChunkBytes}
		s, err := Open(dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		return &reopening{Store: s, t: t, dir: dir, opt: opt, every: 41}
	}, coretest.DiffOptions{Seed: 6, Updates: 3000, ExactVersions: true})
}

// TestConcurrentReadsDuringIngest runs readers against a store that is ingesting and
// committing. Every committed read (live=false) records the version and a hash of the
// pixels it returned; afterwards each is checked against the oracle's content of that
// version, which never changes. Live reads are only checked for errors. Run with -race
// to check the locking.
func TestConcurrentReadsDuringIngest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{MaxChain: 5, DecodedCacheBytes: 8 * imageBytes})
	defer s.Close()
	want := memstore.NewTerrain()
	ds := coretest.DS
	type seen struct {
		tile core.TileCoord
		v    core.Version
		sum  uint64
	}
	reads := make([][]seen, 4)
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
				live := r.Intn(2) == 0
				im, st, err := s.ReadTile(ctx, ds, tc, core.TimeMs(r.Int63n(int64(200*core.Minute))), live)
				if err != nil && !errors.Is(err, core.ErrNoTile) && !errors.Is(err, core.ErrUnknownDatastore) {
					t.Error(err)
					return
				}
				if err == nil && !live && len(reads[g]) < 5000 {
					reads[g] = append(reads[g], seen{tc, st.Version, hashImage(im)})
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
		batch := []core.ChunkUpdate{{DS: ds, Chunk: core.ChunkCoord{X: int32(r.Intn(48) - 24), Y: int32(r.Intn(48) - 24)}, T: now, Pixels: px}}
		if _, err := s.Apply(ctx, batch); err != nil {
			t.Fatal(err)
		}
		want.Apply(ctx, batch)
		if i%40 == 0 {
			if err := s.Commit(ctx, now); err != nil {
				t.Fatal(err)
			}
			want.Commit(ctx, now)
		}
	}
	close(stop)
	wg.Wait()
	checked := 0
	for _, rs := range reads {
		for _, x := range rs {
			wi, err := want.ReadVersion(ctx, ds, x.tile, x.v)
			if err != nil {
				t.Fatalf("oracle %v v%d: %v", x.tile, x.v, err)
			}
			if hashImage(wi) != x.sum {
				t.Fatalf("committed read of %v v%d differs from the oracle", x.tile, x.v)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no committed reads were checked")
	}
	t.Logf("checked %d committed reads", checked)
}

func hashImage(im *core.TileImage) uint64 {
	h := fnv.New64a()
	var b [2]byte
	for _, c := range im.Pix {
		binary.LittleEndian.PutUint16(b[:], c)
		h.Write(b[:])
	}
	return h.Sum64()
}

// TestPaletteCoversLiveContent checks that colours of open-window updates are in the
// palette before any commit (live tiles are encoded with it) and survive a reopen.
func TestPaletteCoversLiveContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	px := coretest.Fill(0x1234)
	px[7] = 0xBEEF
	if _, err := s.Apply(ctx, []core.ChunkUpdate{{DS: coretest.DS, Chunk: core.ChunkCoord{}, T: 5, Pixels: px}}); err != nil {
		t.Fatal(err)
	}
	has := func(s *Store) bool {
		pal, err := s.Palette(ctx, coretest.DS)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint16]bool{}
		for _, c := range pal.Colors {
			seen[c] = true
		}
		return seen[0x1234] && seen[0xBEEF]
	}
	if !has(s) {
		t.Fatal("live colours missing from the palette before commit")
	}
	before, _ := s.Palette(ctx, coretest.DS)
	s.Close()
	if s, err = Open(dir, Options{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, _ := s.Palette(ctx, coretest.DS)
	if fmt.Sprint(before.Colors) != fmt.Sprint(after.Colors) {
		t.Fatalf("palette changed across reopen: %v -> %v", before.Colors, after.Colors)
	}
	im, st, err := s.ReadTile(ctx, coretest.DS, core.TileCoord{}, 5, true)
	if err != nil || !st.Live || im.Block(136) != *px {
		t.Fatalf("live content after reopen: %v %+v", err, st)
	}
}
