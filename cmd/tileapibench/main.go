package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type statsFile struct {
	Tiles []tileStat `json:"tiles"`
}

type tileStat struct {
	Tile                  tileCoord `json:"tile"`
	ChangedRowsStored     uint64    `json:"changed_placements_stored"`
	CompressedBytes       uint64    `json:"compressed_bytes"`
	SnapshotCount         uint64    `json:"snapshot_count"`
	DeltaFrameCount       uint64    `json:"delta_frame_count"`
	MaxReplayEvents       uint64    `json:"max_replay_events_between_snapshots"`
	AverageSnapshotBytes  uint64    `json:"average_snapshot_compressed_bytes"`
	AverageDeltaFrameByte uint64    `json:"average_delta_frame_compressed_bytes"`
}

type tileCoord struct {
	X int
	Y int
}

type metaResponse struct {
	FromSec uint32 `json:"fromSec"`
	ToSec   uint32 `json:"toSec"`
}

type benchResult struct {
	BaseURL          string  `json:"base_url"`
	Requests         int     `json:"requests"`
	Concurrency      int     `json:"concurrency"`
	Seed             int64   `json:"seed"`
	Seconds          float64 `json:"seconds"`
	TilesPerSecond   float64 `json:"tiles_per_second"`
	BytesPerTileMean float64 `json:"bytes_per_tile_mean"`
	P50MS            float64 `json:"p50_ms"`
	P95MS            float64 `json:"p95_ms"`
	P99MS            float64 `json:"p99_ms"`
	MaxMS            float64 `json:"max_ms"`
}

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "tile server base URL")
	statsPath := flag.String("stats", "full.tshd/stats.json", "database stats JSON")
	requests := flag.Int("requests", 2000, "number of tile requests")
	concurrency := flag.Int("concurrency", 16, "parallel HTTP requests")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()

	result, err := run(*baseURL, *statsPath, *requests, *concurrency, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(baseURL, statsPath string, requests, concurrency int, seed int64) (*benchResult, error) {
	if requests <= 0 {
		return nil, fmt.Errorf("requests must be positive")
	}
	if concurrency <= 0 {
		return nil, fmt.Errorf("concurrency must be positive")
	}
	tiles, err := loadTiles(statsPath)
	if err != nil {
		return nil, err
	}
	meta, err := fetchMeta(baseURL)
	if err != nil {
		return nil, err
	}
	urls := make([]string, requests)
	rng := rand.New(rand.NewSource(seed))
	for i := range urls {
		tile := chooseTile(rng, tiles)
		sec := meta.FromSec
		if meta.ToSec > meta.FromSec {
			sec += uint32(rng.Int63n(int64(meta.ToSec-meta.FromSec) + 1))
		}
		urls[i] = fmt.Sprintf("%s/api/tiles/0/%d/%d.png?ts=%d", baseURL, tile.Tile.X, tile.Tile.Y, sec)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	jobs := make(chan string)
	durations := make([]time.Duration, requests)
	var bytesRead uint64
	var completed atomic.Int64
	var errMu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for url := range jobs {
				idx := int(completed.Add(1)) - 1
				t0 := time.Now()
				resp, err := client.Get(url)
				if err != nil {
					setFirstErr(&errMu, &firstErr, err)
					continue
				}
				n, copyErr := io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				durations[idx] = time.Since(t0)
				atomic.AddUint64(&bytesRead, uint64(n))
				if resp.StatusCode != http.StatusOK {
					setFirstErr(&errMu, &firstErr, fmt.Errorf("%s returned %s", url, resp.Status))
				} else if copyErr != nil {
					setFirstErr(&errMu, &firstErr, copyErr)
				} else if closeErr != nil {
					setFirstErr(&errMu, &firstErr, closeErr)
				}
			}
		}()
	}
	for _, url := range urls {
		jobs <- url
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return &benchResult{
		BaseURL:          baseURL,
		Requests:         requests,
		Concurrency:      concurrency,
		Seed:             seed,
		Seconds:          elapsed.Seconds(),
		TilesPerSecond:   float64(requests) / elapsed.Seconds(),
		BytesPerTileMean: float64(bytesRead) / float64(requests),
		P50MS:            ms(percentile(durations, 0.50)),
		P95MS:            ms(percentile(durations, 0.95)),
		P99MS:            ms(percentile(durations, 0.99)),
		MaxMS:            ms(durations[len(durations)-1]),
	}, nil
}

func setFirstErr(mu *sync.Mutex, target *error, err error) {
	mu.Lock()
	defer mu.Unlock()
	if *target == nil {
		*target = err
	}
}

func loadTiles(path string) ([]tileStat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stats statsFile
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}
	if len(stats.Tiles) == 0 {
		return nil, fmt.Errorf("stats file has no tiles")
	}
	var total uint64
	for _, tile := range stats.Tiles {
		total += tile.ChangedRowsStored
	}
	if total == 0 {
		return nil, fmt.Errorf("stats file has no changed tile rows")
	}
	return stats.Tiles, nil
}

func fetchMeta(baseURL string) (metaResponse, error) {
	resp, err := http.Get(baseURL + "/api/meta")
	if err != nil {
		return metaResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metaResponse{}, fmt.Errorf("/api/meta returned %s", resp.Status)
	}
	var meta metaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return metaResponse{}, err
	}
	return meta, nil
}

func chooseTile(rng *rand.Rand, tiles []tileStat) tileStat {
	var total uint64
	for _, tile := range tiles {
		total += tile.ChangedRowsStored
	}
	n := uint64(rng.Int63n(int64(total)))
	for _, tile := range tiles {
		if n < tile.ChangedRowsStored {
			return tile
		}
		n -= tile.ChangedRowsStored
	}
	return tiles[len(tiles)-1]
}

func percentile(values []time.Duration, pct float64) time.Duration {
	idx := int(float64(len(values)-1) * pct)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func ms(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1_000_000
}
