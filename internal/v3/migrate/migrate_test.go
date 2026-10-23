package migrate

import (
	"context"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

var palette = []uint16{0x52AA, 0x4A69, 0x31A6, 0x8C51, 0x2945, 0x6B4D, 0xA534, 0x18C3, 0xF800}

// buildV2 writes a v2 chunk database with random history: two savefiles, two
// surfaces, chunks on both sides of v2 and v3 tile boundaries, flicker (A->B->A),
// rows that repeat a tick, and rounds with large changes that trigger v2 snapshots.
// It returns the path and the number of stored rows (frames).
func buildV2(t *testing.T, rounds int) (string, int) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.tshd")
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, Format: timeshadedb.FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	keys := []timeshadedb.DatastoreKey{
		{SavefileUUID: "save-a", Surface: "nauvis", Force: "player"},
		{SavefileUUID: "save-a", Surface: "vulcanus", Force: "player"},
		{SavefileUUID: "Save.B", Surface: "nauvis", Force: "player"},
	}
	coords := []timeshadedb.ChunkCoord{{X: -9, Y: -9}, {X: -8, Y: -8}, {X: -1, Y: 0}, {X: 0, Y: 0}, {X: 7, Y: 7}, {X: 8, Y: 8}, {X: 15, Y: -17}, {X: 16, Y: 3}, {X: 100, Y: -100}}
	rng := rand.New(rand.NewSource(3))
	rows := 0
	for ki, key := range keys {
		state := map[timeshadedb.ChunkCoord][timeshadedb.ChunkPixelCount]uint16{}
		prev := map[timeshadedb.ChunkCoord][timeshadedb.ChunkPixelCount]uint16{}
		tick := uint64(1000 + 7*ki)
		for r := 0; r < rounds; r++ {
			var batch []timeshadedb.ParsedChunkRow
			for _, c := range coords {
				px, seen := state[c]
				switch {
				case !seen:
					if rng.Intn(3) == 0 && r < rounds/2 {
						continue // charted later
					}
					for i := range px {
						px[i] = palette[rng.Intn(len(palette))]
					}
				case rng.Intn(2) == 0:
					continue
				case rng.Intn(6) == 0:
					px = prev[c] // flicker back
				default:
					n := 1 + rng.Intn(10)
					if rng.Intn(12) == 0 {
						n = 400
					}
					for i := 0; i < n; i++ {
						px[rng.Intn(len(px))] = palette[rng.Intn(len(palette))]
					}
				}
				prev[c] = state[c]
				state[c] = px
				rowTick := tick
				if rng.Intn(15) == 0 && r > 0 {
					rowTick -= 300 // same tick as the previous round's rows
				}
				batch = append(batch, timeshadedb.ParsedChunkRow{Key: key, Tick: rowTick, Chunk: c, Pixels: px})
			}
			if len(batch) > 0 {
				res, err := db.IngestChunkRows(ctx, batch)
				if err != nil {
					t.Fatal(err)
				}
				_ = res
			}
			tick += 300 // 5 s: 12 rounds per 60 s window
		}
	}
	stats, err := db.ChunkStats()
	if err != nil {
		t.Fatal(err)
	}
	rows = int(stats.SnapshotCount + stats.DeltaFrameCount)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, rows
}

func TestClockRoundTrip(t *testing.T) {
	for _, c := range []Clock{{EpochMs: 0, UPS: 60}, {EpochMs: 1_700_000_000_123, UPS: 60}, {EpochMs: -5, UPS: 30}, {EpochMs: 42, UPS: 1000}} {
		for tick := uint64(0); tick < 5000; tick += 7 {
			tm := c.TimeOf(tick)
			got, ok := c.LastTickBefore(tm + 1)
			if !ok || c.TimeOf(got) > tm || c.TimeOf(got+1) <= tm {
				t.Fatalf("clock %+v: LastTickBefore(TimeOf(%d)+1) = %d, %v", c, tick, got, ok)
			}
			if got, ok := c.LastTickBefore(tm); ok && c.TimeOf(got) >= tm {
				t.Fatalf("clock %+v: LastTickBefore(%v) = %d with time %v", c, tm, got, c.TimeOf(got))
			}
		}
		if _, ok := c.LastTickBefore(c.EpochMs); ok {
			t.Fatalf("clock %+v: a tick before the epoch", c)
		}
	}
	at := time.UnixMilli(1_760_000_000_000)
	c := AnchorClock(3600, at, 60)
	if c.TimeOf(3600) != core.FromTime(at) {
		t.Fatalf("anchored clock puts tick 3600 at %v, want %v", c.TimeOf(3600), core.FromTime(at))
	}
}

func TestMigrateIsPixelIdenticalAtRandomTicks(t *testing.T) {
	ctx := context.Background()
	path, frames := buildV2(t, 240)
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, window := range []core.TimeMs{core.DefaultTerrainWindow, 5 * core.Second} {
		dst := memstore.NewTerrain()
		opts := Options{
			Clock:  FixedClock(Clock{EpochMs: 1_760_000_000_000, UPS: 60}),
			Window: window,
		}
		stats, err := Migrate(ctx, src, dst, opts)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Rows != frames {
			t.Fatalf("window %v: migrated %d rows, v2 holds %d frames", window, stats.Rows, frames)
		}
		if stats.Applied.Accepted != frames || stats.Applied.Late != 0 || stats.Applied.Invalid != 0 {
			t.Fatalf("window %v: apply result %+v", window, stats.Applied)
		}
		if len(stats.Datastores) != 3 {
			t.Fatalf("datastores = %d", len(stats.Datastores))
		}
		infos, err := dst.Datastores(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(infos) != 3 || infos[0].Window != window {
			t.Fatalf("v3 datastores = %+v", infos)
		}
		for _, info := range infos {
			if !strings.Contains(info.ID, ".nauvis.") && !strings.Contains(info.ID, ".vulcanus.") {
				t.Fatalf("unexpected id %s", info.ID)
			}
		}
		report, err := Verify(ctx, src, dst, opts, VerifyOptions{Samples: 20, Seed: 1, Window: window})
		if err != nil {
			t.Fatal(err)
		}
		chunks := 0
		for _, c := range report.Checks {
			chunks += c.Chunks
		}
		t.Logf("window %d ms: %d rows, %d checks, %d chunk comparisons with v2 data, %d bad pixels", int64(window), stats.Rows, len(report.Checks), chunks, report.BadPixels)
		if !report.OK() {
			t.Fatalf("window %v: migrated store differs from v2: %+v", window, report.Mismatches)
		}
		if len(report.Checks) != 3*20 || chunks == 0 {
			t.Fatalf("window %v: %d checks, %d chunk comparisons", window, len(report.Checks), chunks)
		}
	}
}

// corruptStore flips one pixel of chunk (0, 0) of one datastore in every read.
type corruptStore struct {
	core.TerrainStore
	ds core.DatastoreKey
}

func (c corruptStore) ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error) {
	im, st, err := c.TerrainStore.ReadTile(ctx, ds, tile, t, live)
	if err == nil && ds == c.ds {
		if want, block := (core.ChunkCoord{}).Tile(); want == tile {
			im.Pix[core.TilePixelIndex(block, 5)] ^= 1
		}
	}
	return im, st, err
}

// TestVerifyDetectsDifferences makes sure Verify is not vacuous.
func TestVerifyDetectsDifferences(t *testing.T) {
	ctx := context.Background()
	path, _ := buildV2(t, 60)
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	opts := Options{Clock: FixedClock(Clock{EpochMs: 1_000_000, UPS: 60})}
	dst := memstore.NewTerrain()
	if _, err := Migrate(ctx, src, dst, opts); err != nil {
		t.Fatal(err)
	}
	if report, err := Verify(ctx, src, dst, opts, VerifyOptions{Samples: 5}); err != nil || !report.OK() {
		t.Fatalf("clean store: %v %+v", err, report.Mismatches)
	}
	key := core.DatastoreKey{Map: "save-a", Surface: "nauvis", Force: "player"}
	report, err := Verify(ctx, src, corruptStore{TerrainStore: dst, ds: key}, opts, VerifyOptions{Samples: 5})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() || len(report.Mismatches) == 0 || report.Mismatches[0].Chunk != (core.ChunkCoord{}) || report.Mismatches[0].FirstPos != 5 {
		t.Fatalf("corrupted pixel not reported: %+v", report.Mismatches)
	}
	// An empty store reads as uncharted everywhere.
	report, err = Verify(ctx, src, memstore.NewTerrain(), opts, VerifyOptions{Samples: 5})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("Verify passed an empty store")
	}
	// A different clock compares different instants and must fail too.
	shifted := Options{Clock: FixedClock(Clock{EpochMs: 1_000_000 - 2*core.DefaultTerrainWindow, UPS: 60})}
	if report, err := Verify(ctx, src, dst, shifted, VerifyOptions{Samples: 5}); err != nil || report.OK() {
		t.Fatalf("shifted clock: %v, ok=%v", err, report.OK())
	}
	// Data in a tile without any v2 chunk is reported, even though no compared pixel
	// differs.
	far := core.ChunkCoord{X: 4000, Y: -4000}
	var px core.ChunkPixels
	px[0] = 0xF800
	if res, err := dst.Apply(ctx, []core.ChunkUpdate{{DS: key, Chunk: far, T: 1 << 41, Pixels: &px}}); err != nil || res.Accepted != 1 {
		t.Fatalf("stray apply: %v %+v", err, res)
	}
	if err := dst.Commit(ctx, 1<<42); err != nil {
		t.Fatal(err)
	}
	report, err = Verify(ctx, src, dst, opts, VerifyOptions{Samples: 5})
	if err != nil {
		t.Fatal(err)
	}
	farTile, _ := far.Tile()
	if report.OK() || report.BadPixels != 0 || report.ExtraTiles != 1 || len(report.Extra) != 1 || report.Extra[0].Tile != farTile {
		t.Fatalf("extra tile not reported: bad=%d extra=%d %+v", report.BadPixels, report.ExtraTiles, report.Extra)
	}
}

func TestMigrateRefusesCollisionsAndExistingDatastores(t *testing.T) {
	ctx := context.Background()
	path, _ := buildV2(t, 10)
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	clock := FixedClock(Clock{EpochMs: 1_000_000})
	_, err = Migrate(ctx, src, memstore.NewTerrain(), Options{
		Clock: clock,
		MapKey: func(k timeshadedb.DatastoreKey) core.DatastoreKey {
			return core.DatastoreKey{Map: "one", Surface: k.Surface, Force: k.Force}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "both map to") {
		t.Fatalf("collision error = %v", err)
	}
	dst := memstore.NewTerrain()
	if err := dst.EnsureDatastore(ctx, core.DatastoreKey{Map: "save-a", Surface: "nauvis", Force: "player"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, src, dst, Options{Clock: clock}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing datastore error = %v", err)
	}
	if _, err := Migrate(ctx, src, dst, Options{}); err == nil {
		t.Fatal("missing clock accepted")
	}
}

func TestAutoClocksAnchorAtProgressTime(t *testing.T) {
	path, _ := buildV2(t, 5)
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	clocks, err := AutoClocks(src, 60)
	if err != nil {
		t.Fatal(err)
	}
	c, err := clocks("save-a")
	if err != nil {
		t.Fatal(err)
	}
	key := timeshadedb.DatastoreKey{SavefileUUID: "save-a", Surface: "vulcanus", Force: "player"}
	latest, _, err := src.ChunkDatastoreLatestTick(key)
	if err != nil {
		t.Fatal(err)
	}
	at, err := src.ChunkDatastoreProgressTime(key)
	if err != nil {
		t.Fatal(err)
	}
	if d := c.TimeOf(latest) - core.FromTime(at); d < -1 || d > 1 {
		t.Fatalf("latest tick at %v, progress written at %v", c.TimeOf(latest), core.FromTime(at))
	}
	if _, err := clocks("unknown"); err == nil {
		t.Fatal("clock for an unknown savefile")
	}
}
