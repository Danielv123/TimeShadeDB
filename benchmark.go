package timeshadedb

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"time"
)

type BenchmarkOptions struct {
	Queries int
	Seed    int64
}

type BenchmarkStats struct {
	Queries int               `json:"queries"`
	Seed    int64             `json:"seed"`
	FromSec uint32            `json:"from_sec"`
	ToSec   uint32            `json:"to_sec"`
	Results []BenchmarkResult `json:"results"`
}

type BenchmarkResult struct {
	Scenario string  `json:"scenario"`
	Queries  int     `json:"queries"`
	P50MS    float64 `json:"query_p50_ms"`
	P95MS    float64 `json:"query_p95_ms"`
	P99MS    float64 `json:"query_p99_ms"`
	MaxMS    float64 `json:"query_max_ms"`
}

func (db *DB) Benchmark(ctx context.Context, opts BenchmarkOptions) (*BenchmarkStats, error) {
	queries := opts.Queries
	if queries <= 0 {
		queries = 100
	}
	seed := opts.Seed
	if seed == 0 {
		seed = 1
	}
	fromSec, toSec := db.timeRange()
	rng := rand.New(rand.NewSource(seed))
	stats := &BenchmarkStats{Queries: queries, Seed: seed, FromSec: fromSec, ToSec: toSec}
	hotTile := db.hotTile()
	scenarios := []struct {
		name string
		run  func(context.Context) error
	}{
		{
			name: "single_random_tile_random_timestamp",
			run: func(ctx context.Context) error {
				sec := randomSec(rng, fromSec, toSec)
				tile := TileCoord{X: rng.Intn(TileCols), Y: rng.Intn(TileRows)}
				_, err := db.TileAt(ctx, TileAtOptions{Timestamp: time.Unix(int64(sec), 0).UTC(), Tile: tile})
				return err
			},
		},
		{
			name: "all_tiles_random_timestamp",
			run: func(ctx context.Context) error {
				sec := randomSec(rng, fromSec, toSec)
				return db.queryAllTiles(ctx, sec)
			},
		},
		{
			name: "hot_tile_near_peak",
			run: func(ctx context.Context) error {
				_, err := db.TileAt(ctx, TileAtOptions{Timestamp: time.Unix(int64(toSec), 0).UTC(), Tile: hotTile})
				return err
			},
		},
		{
			name: "latest_tile_query",
			run: func(ctx context.Context) error {
				tile := TileCoord{X: rng.Intn(TileCols), Y: rng.Intn(TileRows)}
				_, err := db.TileAt(ctx, TileAtOptions{Timestamp: time.Unix(int64(toSec), 0).UTC(), Tile: tile})
				return err
			},
		},
		{
			name: "oldest_tile_query",
			run: func(ctx context.Context) error {
				tile := TileCoord{X: rng.Intn(TileCols), Y: rng.Intn(TileRows)}
				_, err := db.TileAt(ctx, TileAtOptions{Timestamp: time.Unix(int64(fromSec), 0).UTC(), Tile: tile})
				return err
			},
		},
	}
	for _, scenario := range scenarios {
		result, err := runBenchmarkScenario(ctx, scenario.name, queries, scenario.run)
		if err != nil {
			return nil, err
		}
		stats.Results = append(stats.Results, result)
	}
	return stats, nil
}

func runBenchmarkScenario(ctx context.Context, name string, queries int, run func(context.Context) error) (BenchmarkResult, error) {
	durations := make([]time.Duration, 0, queries)
	for i := 0; i < queries; i++ {
		if err := ctx.Err(); err != nil {
			return BenchmarkResult{}, err
		}
		start := time.Now()
		if err := run(ctx); err != nil {
			return BenchmarkResult{}, err
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return BenchmarkResult{
		Scenario: name,
		Queries:  queries,
		P50MS:    durationMS(percentileDuration(durations, 0.50)),
		P95MS:    durationMS(percentileDuration(durations, 0.95)),
		P99MS:    durationMS(percentileDuration(durations, 0.99)),
		MaxMS:    durationMS(durations[len(durations)-1]),
	}, nil
}

func (db *DB) queryAllTiles(ctx context.Context, sec uint32) error {
	var wg sync.WaitGroup
	errCh := make(chan error, TileCount)
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			wg.Add(1)
			go func(x, y int) {
				defer wg.Done()
				_, err := db.TileAt(ctx, TileAtOptions{Timestamp: time.Unix(int64(sec), 0).UTC(), Tile: TileCoord{X: x, Y: y}})
				errCh <- err
			}(x, y)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) timeRange() (uint32, uint32) {
	var min uint32
	var max uint32
	for _, t := range db.tiles {
		if t == nil {
			continue
		}
		for _, s := range t.index.snapshots {
			if s.TimestampSec != 0 && (min == 0 || s.TimestampSec < min) {
				min = s.TimestampSec
			}
			if s.TimestampSec > max {
				max = s.TimestampSec
			}
		}
		for _, d := range t.index.deltas {
			if d.MinTimestampSec != 0 && (min == 0 || d.MinTimestampSec < min) {
				min = d.MinTimestampSec
			}
			if d.MaxTimestampSec > max {
				max = d.MaxTimestampSec
			}
		}
	}
	if min == 0 {
		min = max
	}
	return min, max
}

func (db *DB) hotTile() TileCoord {
	var best TileCoord
	var bestEvents uint64
	for _, t := range db.tiles {
		if t == nil {
			continue
		}
		var events uint64
		for _, d := range t.index.deltas {
			events += uint64(d.EventCount)
		}
		if events > bestEvents {
			bestEvents = events
			best = TileCoord{X: t.x, Y: t.y}
		}
	}
	return best
}

func randomSec(rng *rand.Rand, from, to uint32) uint32 {
	if to <= from {
		return from
	}
	return from + uint32(rng.Int63n(int64(to-from)+1))
}

func percentileDuration(values []time.Duration, pct float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)-1) * pct)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func durationMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1_000_000
}
