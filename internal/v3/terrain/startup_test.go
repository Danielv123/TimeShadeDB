package terrain

import (
	"context"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
)

// buildDB writes a database with the given number of tiles, each with windows windows
// of one changed chunk, and returns its directory.
func buildDB(t *testing.T, tiles, windows int) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true, NoWAL: true, OpenWindowBytes: -1, DecodedCacheBytes: int64(tiles+8) * imageBytes, MaxOpenSeries: tiles})
	if err != nil {
		t.Fatal(err)
	}
	side := 1
	for side*side < tiles {
		side++
	}
	base := core.TimeMs(1_791_630_000_000)
	for w := 0; w < windows; w++ {
		ups := make([]core.ChunkUpdate, 0, tiles)
		for i := 0; i < tiles; i++ {
			tc := core.TileCoord{X: int32(i%side - side/2), Y: int32(i/side - side/2)}
			ups = append(ups, core.ChunkUpdate{DS: coretest.DS, Chunk: tc.Chunk(uint8(w)), T: base + core.TimeMs(w)*core.Minute, Pixels: coretest.Fill(uint16(1 + (w+i)%200))})
		}
		if _, err := s.Apply(ctx, ups); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(ctx, base+core.TimeMs(w+1)*core.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func timeOpen(t *testing.T, dir string) (time.Duration, int) {
	t.Helper()
	best := time.Duration(1 << 62)
	var n int
	for i := 0; i < 3; i++ {
		t0 := hrNow()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		best = min(best, hrSince(t0))
		n = s.Stats().Tiles
		s.Close()
	}
	return best, n
}

// TestStartupScaling reports how open time grows with the number of tiles and with
// history length. Startup reads every tile's index but decodes no history, so it should
// be linear in tiles and only weakly dependent on history.
func TestStartupScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type cfg struct{ tiles, windows int }
	var res []time.Duration
	for _, c := range []cfg{{200, 10}, {2000, 10}, {200, 100}} {
		d, n := timeOpen(t, buildDB(t, c.tiles, c.windows))
		if n != c.tiles {
			t.Fatalf("opened %d tiles, want %d", n, c.tiles)
		}
		t.Logf("%d tiles x %d versions: open %v (%.1f us/tile)", c.tiles, c.windows, d, float64(d.Microseconds())/float64(c.tiles))
		res = append(res, d)
	}
	// Ten times the history may not make startup ten times slower.
	if res[2] > 5*res[0] {
		t.Errorf("startup grows with history: %v for 10x history vs %v", res[2], res[0])
	}
}
