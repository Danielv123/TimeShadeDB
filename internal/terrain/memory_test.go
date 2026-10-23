package terrain

import (
	"context"
	"math/rand"
	"runtime"
	"testing"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
)

func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// TestHeapFlatOverHistory ingests a long flicker history (Zipf-like: a few hot chunks
// change every window, the rest rarely) with small budgets and checks that the heap
// after GC does not grow with history: no history is held in memory, and caches and
// indexes stay within their budgets.
func TestHeapFlatOverHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	ctx := context.Background()
	opt := Options{NoSync: true, DecodedCacheBytes: 16 * imageBytes, MaxIndexBytes: 256 << 10, MaxOpenSeries: 32}
	s := openTemp(t, opt)
	defer s.Close()
	r := rand.New(rand.NewSource(3))
	const windows = 1500
	const tiles = 6 // 6 x 6 z0 tiles
	colours := []uint16{0x8C71, 0xF8C3, 0x4208, 0x07E0, 0xFFFF}
	base := core.TimeMs(1_791_630_000_000)
	var mid uint64
	for w := 0; w < windows; w++ {
		var ups []core.ChunkUpdate
		for i := 0; i < 40; i++ {
			var c core.ChunkCoord
			if r.Intn(10) < 8 { // hot chunks: 80% of updates on 30 chunks
				h := r.Intn(30)
				c = core.ChunkCoord{X: int32(h*5%(tiles*16)) - 48, Y: int32(h*11%(tiles*16)) - 48}
			} else {
				c = core.ChunkCoord{X: int32(r.Intn(tiles*16)) - 48, Y: int32(r.Intn(tiles*16)) - 48}
			}
			px := coretest.Fill(0x8C71)
			for j := r.Intn(30); j > 0; j-- {
				px[r.Intn(core.ChunkPixelCount)] = colours[r.Intn(len(colours))]
			}
			ups = append(ups, core.ChunkUpdate{DS: coretest.DS, Chunk: c, T: base + core.TimeMs(w)*core.Minute + core.TimeMs(r.Intn(60_000)), Pixels: px})
		}
		if _, err := s.Apply(ctx, ups); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(ctx, base+core.TimeMs(w)*core.Minute); err != nil {
			t.Fatal(err)
		}
		if w == windows/3 {
			mid = heapAfterGC()
		}
	}
	end := heapAfterGC()
	st := s.Stats()
	t.Logf("heap after %d windows: %d KB, after %d: %d KB; %d versions written, %d keyframes, cache %d KB, idle index %d KB",
		windows/3, mid>>10, windows, end>>10, st.DeltasWritten, st.KeyframesWritten, st.DecodedCacheBytes>>10, st.Series.IdleIndexBytes>>10)
	if end > mid+mid/20+(2<<20) {
		t.Fatalf("heap grew from %d KB to %d KB", mid>>10, end>>10)
	}
	if st.DecodedCacheBytes > opt.DecodedCacheBytes || st.Series.IdleIndexBytes > opt.MaxIndexBytes {
		t.Fatalf("budgets exceeded: %+v", st)
	}
}
