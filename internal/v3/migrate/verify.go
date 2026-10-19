package migrate

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
)

// VerifyOptions control Verify.
type VerifyOptions struct {
	// Samples is the number of random ticks checked per datastore (0: 20).
	Samples int
	// Seed seeds the tick choice.
	Seed int64
	// Window must match the migration's window (0: core.DefaultTerrainWindow).
	Window core.TimeMs
	// MaxMismatches stops collecting mismatch details after this many (0: 10).
	MaxMismatches int
}

// Check is one compared instant: the v3 store at window boundary T against the v2
// database at Tick, the last tick before T.
type Check struct {
	From      timeshadedb.DatastoreKey `json:"from"`
	To        core.DatastoreKey        `json:"to"`
	Tick      uint64                   `json:"tick"`
	T         core.TimeMs              `json:"t"`
	Tiles     int                      `json:"tiles"`
	Chunks    int                      `json:"chunks"`
	BadPixels int                      `json:"bad_pixels"`
}

// Mismatch describes one differing chunk.
type Mismatch struct {
	Check     Check           `json:"check"`
	Chunk     core.ChunkCoord `json:"chunk"`
	BadPixels int             `json:"bad_pixels"`
	FirstPos  int             `json:"first_pos"`
	V2        uint16          `json:"v2"`
	V3        uint16          `json:"v3"`
}

// VerifyReport sums a verification.
type VerifyReport struct {
	Checks     []Check    `json:"checks"`
	Mismatches []Mismatch `json:"mismatches,omitempty"`
	BadPixels  int        `json:"bad_pixels"`
}

// OK reports whether every compared pixel matched.
func (r VerifyReport) OK() bool { return r.BadPixels == 0 }

// Verify compares src and dst at random instants. For each datastore it picks random
// ticks between the first and last stored tick (always including both), moves each
// to the next window boundary T = WindowEnd(TimeOf(tick)), and compares every chunk
// of every v3 tile that holds v2 data: v3 ReadTile at T (committed content only)
// against v2 ChunksAt at the last tick before T. Blocks without a v2 chunk must be
// uncharted in v3.
func Verify(ctx context.Context, src *timeshadedb.DB, dst core.TerrainStore, opts Options, vopts VerifyOptions) (VerifyReport, error) {
	var report VerifyReport
	if opts.Clock == nil {
		return report, errors.New("migrate: Options.Clock is required")
	}
	mapKey := opts.MapKey
	if mapKey == nil {
		mapKey = DefaultMapKey
	}
	samples := vopts.Samples
	if samples <= 0 {
		samples = 20
	}
	window := vopts.Window
	if window <= 0 {
		window = core.DefaultTerrainWindow
	}
	maxMismatches := vopts.MaxMismatches
	if maxMismatches <= 0 {
		maxMismatches = 10
	}
	rng := rand.New(rand.NewSource(vopts.Seed))
	keys, err := Datastores(src)
	if err != nil {
		return report, err
	}
	for _, from := range keys {
		to := mapKey(from)
		clock, err := opts.Clock(from.SavefileUUID)
		if err != nil {
			return report, err
		}
		coords, err := src.ChunkCoords(from)
		if err != nil {
			return report, err
		}
		if len(coords) == 0 {
			continue
		}
		// v3 tiles and the v2 chunks inside them.
		tiles := map[core.TileCoord][]timeshadedb.ChunkCoord{}
		for _, c := range coords {
			tile, _ := core.ChunkCoord{X: c.X, Y: c.Y}.Tile()
			tiles[tile] = append(tiles[tile], c)
		}
		tileList := make([]core.TileCoord, 0, len(tiles))
		for tile := range tiles {
			tileList = append(tileList, tile)
		}
		sort.Slice(tileList, func(i, j int) bool {
			if tileList[i].Y != tileList[j].Y {
				return tileList[i].Y < tileList[j].Y
			}
			return tileList[i].X < tileList[j].X
		})
		first, last, ok, err := src.ChunkTickRange(from)
		if err != nil {
			return report, err
		}
		if !ok {
			continue
		}
		ticks := []uint64{first, last}
		for len(ticks) < samples {
			ticks = append(ticks, first+uint64(rng.Int63n(int64(last-first)+1)))
		}
		sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
		for _, tick := range ticks {
			t := core.WindowEnd(clock.TimeOf(tick), window)
			v2Tick, any := clock.LastTickBefore(t)
			check := Check{From: from, To: to, Tick: v2Tick, T: t, Tiles: len(tileList)}
			for _, tile := range tileList {
				if err := ctx.Err(); err != nil {
					return report, err
				}
				var v2 map[timeshadedb.ChunkCoord]*timeshadedb.ChunkResult
				if any {
					v2, err = src.ChunksAt(ctx, from, v2Tick, tiles[tile])
					if err != nil {
						return report, err
					}
				}
				im, _, err := dst.ReadTile(ctx, to, tile, t, false)
				if errors.Is(err, core.ErrNoTile) || errors.Is(err, core.ErrUnknownDatastore) {
					im, err = core.NewTileImage(), nil
				}
				if err != nil {
					return report, fmt.Errorf("verify %s %v at %v: %w", to.ID(), tile, t, err)
				}
				for block := 0; block < core.TileBlocks; block++ {
					c3 := tile.Chunk(uint8(block))
					got := im.Block(uint8(block))
					var want [core.ChunkPixelCount]uint16
					if res := v2[timeshadedb.ChunkCoord{X: c3.X, Y: c3.Y}]; res != nil {
						copy(want[:], res.Pixels)
						check.Chunks++
					}
					bad, firstPos := 0, -1
					for i := range want {
						if want[i] != got[i] {
							bad++
							if firstPos < 0 {
								firstPos = i
							}
						}
					}
					if bad > 0 {
						check.BadPixels += bad
						if len(report.Mismatches) < maxMismatches {
							report.Mismatches = append(report.Mismatches, Mismatch{Check: check, Chunk: c3, BadPixels: bad, FirstPos: firstPos, V2: want[firstPos], V3: got[firstPos]})
						}
					}
				}
			}
			report.BadPixels += check.BadPixels
			report.Checks = append(report.Checks, check)
		}
	}
	return report, nil
}
