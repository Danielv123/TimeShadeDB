package terrain

import (
	"context"
	"encoding/json"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/terrain/replay"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return v
	}
	return def
}

// replayReport is what TestReplayTestdata measures (written as JSON to
// TSDB_REPLAY_OUT when set).
type replayReport struct {
	Config        map[string]any    `json:"config"`
	CPU           string            `json:"cpu"`
	Updates       int               `json:"updates"`
	Batch         int               `json:"batch"`
	StoreSeconds  float64           `json:"store_seconds"`
	ApplySeconds  float64           `json:"apply_seconds"`
	CommitSeconds float64           `json:"commit_seconds"`
	WallSeconds   float64           `json:"wall_seconds"`
	UpdatesPerSec float64           `json:"updates_per_sec_store"`
	Late          uint64            `json:"late"`
	Deltas        uint64            `json:"deltas"`
	Keyframes     uint64            `json:"keyframes"`
	PeakOpenBytes int64             `json:"peak_open_window_bytes"`
	PeakHeap      uint64            `json:"peak_heap_alloc_bytes"`
	HeapAfter     uint64            `json:"heap_after_ingest_gc_bytes"`
	Datastores    []dsReport        `json:"datastores"`
	TotalBytes    int64             `json:"total_bytes"`
	TerrainBytes  int64             `json:"terrain_bytes"`
	ChunkHours    float64           `json:"chunk_hours"`
	BytesPerCH    float64           `json:"bytes_per_chunk_hour"`
	StartupMs     float64           `json:"startup_ms"`
	StartupMs2    float64           `json:"startup_ms_second_open"`
	StartupTiles  int               `json:"startup_tiles"`
	HeapAfterOpen uint64            `json:"heap_after_open_bytes"`
	Reads         map[string]latRep `json:"reads"`
	Verified      int               `json:"verified_chunks"`
}

type dsReport struct {
	ID         string  `json:"id"`
	Tiles      int     `json:"tiles"`
	Chunks     int     `json:"chunks"`
	Hours      float64 `json:"hours"`
	Palette    uint32  `json:"palette"`
	Bytes      int64   `json:"bytes"`
	BytesPerCH float64 `json:"bytes_per_chunk_hour"`
}

type latRep struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func latencies(d []time.Duration) latRep {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	q := func(p float64) float64 {
		if len(d) == 0 {
			return 0
		}
		return float64(d[min(len(d)-1, int(p*float64(len(d))))].Microseconds()) / 1000
	}
	return latRep{N: len(d), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: q(1)}
}

func dirBytes(t *testing.T, dir string, match func(string) bool) int64 {
	var n int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && match(filepath.ToSlash(p)) {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// TestReplayTestdata replays the illisis test data (TSDB_TESTDATA = the directory
// holding maps.json.gz) through the store and reports ingest rate, disk use per chunk-
// hour, startup time and cold read latency. Knobs: TSDB_REPLAY_FILTER (substring of the
// datastore id), TSDB_REPLAY_WINDOW (seconds, default 60), TSDB_REPLAY_KF (keyframe
// factor), TSDB_REPLAY_CAP (max chain), TSDB_REPLAY_LEVEL (1 fastest .. 4 best),
// TSDB_REPLAY_BATCH, TSDB_REPLAY_NOSYNC=1, TSDB_REPLAY_NOWAL=1, TSDB_REPLAY_DB (keep
// the database there), TSDB_REPLAY_OUT (JSON report path), TSDB_REPLAY_READS.
func TestReplayTestdata(t *testing.T) {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set")
	}
	ctx := context.Background()
	filter := os.Getenv("TSDB_REPLAY_FILTER")
	window := core.TimeMs(envInt("TSDB_REPLAY_WINDOW", 60)) * core.Second
	batch := envInt("TSDB_REPLAY_BATCH", 1024)
	opt := Options{
		Window:         window,
		KeyframeFactor: envFloat("TSDB_REPLAY_KF", 0),
		MaxChain:       envInt("TSDB_REPLAY_CAP", 0),
		Level:          zstd.EncoderLevel(envInt("TSDB_REPLAY_LEVEL", 0)),
		NoSync:         os.Getenv("TSDB_REPLAY_NOSYNC") == "1",
		NoWAL:          os.Getenv("TSDB_REPLAY_NOWAL") == "1",
	}
	dir := os.Getenv("TSDB_REPLAY_DB")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	areas, err := replay.Scan(root, func(k core.DatastoreKey) bool { return filter == "" || strings.Contains(k.ID(), filter) })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := replay.NewStream(areas)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	rep := replayReport{Batch: batch, CPU: runtime.GOARCH + "/" + runtime.GOOS + " GOMAXPROCS=" + strconv.Itoa(runtime.GOMAXPROCS(0))}
	type span struct{ first, last core.TimeMs }
	spans := map[core.DatastoreKey]*span{}
	ups := make([]core.ChunkUpdate, batch)
	bufs := make([]core.ChunkPixels, batch)
	var applyDur, commitDur time.Duration
	var lastT core.TimeMs
	wall := time.Now()
	for {
		n := 0
		for n < batch {
			ok, err := stream.Next(&ups[n], &bufs[n])
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			ups[n].Pixels = &bufs[n]
			sp := spans[ups[n].DS]
			if sp == nil {
				sp = &span{ups[n].T, ups[n].T}
				spans[ups[n].DS] = sp
			}
			sp.last = max(sp.last, ups[n].T)
			lastT = max(lastT, ups[n].T)
			n++
		}
		if n == 0 {
			break
		}
		t0 := hrNow()
		if err := s.Commit(ctx, ups[0].T); err != nil {
			t.Fatal(err)
		}
		commitDur += hrSince(t0)
		t1 := hrNow()
		res, err := s.Apply(ctx, ups[:n])
		if err != nil {
			t.Fatal(err)
		}
		applyDur += hrSince(t1)
		rep.Updates += n
		if res.Invalid > 0 {
			t.Fatalf("invalid updates: %+v", res)
		}
		if ob := s.Stats().OpenWindowBytes; ob > rep.PeakOpenBytes {
			rep.PeakOpenBytes = ob
		}
		if (rep.Updates/batch)%256 == 0 {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			rep.PeakHeap = max(rep.PeakHeap, ms.HeapAlloc)
		}
	}
	t0 := hrNow()
	if err := s.Commit(ctx, lastT+window); err != nil {
		t.Fatal(err)
	}
	commitDur += hrSince(t0)
	rep.WallSeconds = time.Since(wall).Seconds()
	rep.ApplySeconds, rep.CommitSeconds = applyDur.Seconds(), commitDur.Seconds()
	rep.StoreSeconds = (applyDur + commitDur).Seconds()
	rep.UpdatesPerSec = float64(rep.Updates) / rep.StoreSeconds
	st := s.Stats()
	rep.Late, rep.Deltas, rep.Keyframes = st.Late, st.DeltasWritten, st.KeyframesWritten

	// Every chunk's latest content must equal the source's final state.
	final := stream.FinalState()
	for ds, chunks := range final {
		byTile := map[core.TileCoord]*core.TileImage{}
		for c, px := range chunks {
			tc, b := c.Tile()
			im := byTile[tc]
			if im == nil {
				im, _, err = s.ReadTile(ctx, ds, tc, lastT+window, false)
				if err != nil {
					t.Fatal(err)
				}
				byTile[tc] = im
			}
			if im.Block(b) != *px {
				t.Fatalf("%v chunk %v: stored latest differs from the source", ds, c)
			}
			rep.Verified++
		}
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rep.HeapAfter = ms.HeapAlloc
	if p := os.Getenv("TSDB_REPLAY_HEAPPROF"); p != "" {
		if f, err := os.Create(p); err == nil {
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Disk use.
	dss, _ := os.ReadDir(filepath.Join(dir, "datastores"))
	for _, e := range dss {
		key, err := core.ParseDatastoreID(e.Name())
		if err != nil {
			continue
		}
		p := filepath.Join(dir, "datastores", e.Name())
		b := dirBytes(t, p, func(f string) bool {
			return strings.HasSuffix(f, ".blk") || strings.HasSuffix(f, ".idx") || strings.HasSuffix(f, "palette.bin")
		})
		sp := spans[key]
		hours := float64(sp.last-sp.first) / float64(core.Hour)
		chunks := len(final[key])
		tiles, _ := os.ReadDir(filepath.Join(p, "tiles"))
		dr := dsReport{ID: e.Name(), Tiles: len(tiles) / 2, Chunks: chunks, Hours: hours, Bytes: b}
		if pb, err := os.ReadFile(filepath.Join(p, "palette.bin")); err == nil {
			dr.Palette = uint32((len(pb) - 8) / 2)
		}
		if ch := float64(chunks) * hours; ch > 0 {
			dr.BytesPerCH = float64(b) / ch
			rep.ChunkHours += ch
		}
		rep.TerrainBytes += b
		rep.Datastores = append(rep.Datastores, dr)
	}
	rep.TotalBytes = dirBytes(t, dir, func(string) bool { return true })
	rep.BytesPerCH = float64(rep.TerrainBytes) / rep.ChunkHours

	// Startup: reopen (reads every tile index, replays the empty WAL).
	runtime.GC()
	t0 = hrNow()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep.StartupMs = float64(hrSince(t0).Microseconds()) / 1000
	// A second open shows the steady-state cost (the first open after writing the
	// files has been seen to be much slower on Windows).
	s.Close()
	t0 = hrNow()
	if s, err = Open(dir, Options{}); err != nil {
		t.Fatal(err)
	}
	rep.StartupMs2 = float64(hrSince(t0).Microseconds()) / 1000
	runtime.GC()
	runtime.ReadMemStats(&ms)
	rep.HeapAfterOpen = ms.HeapAlloc
	rep.StartupTiles = s.Stats().Tiles

	// Cold reads: random (tile, t) with the decoded cache emptied before each read.
	type tt struct {
		ds   core.DatastoreKey
		tile core.TileCoord
		info core.TileInfo
	}
	var tiles []tt
	all, _ := s.Datastores(ctx)
	for _, d := range all {
		infos, _ := s.ListTiles(ctx, d.Key, 0)
		for _, in := range infos {
			if in.Version > 0 {
				tiles = append(tiles, tt{d.Key, in.Tile, in})
			}
		}
	}
	r := rand.New(rand.NewSource(1))
	nReads := envInt("TSDB_REPLAY_READS", 2000)
	var cold, warm, latest, versionAt []time.Duration
	for i := 0; i < nReads && len(tiles) > 0; i++ {
		x := tiles[r.Intn(len(tiles))]
		span := int64(x.info.LastT-x.info.FirstT) + 1
		at := x.info.FirstT + core.TimeMs(r.Int63n(span))
		s.DropCaches()
		t0 := hrNow()
		if _, _, err := s.ReadTile(ctx, x.ds, x.tile, at, false); err != nil {
			t.Fatal(err)
		}
		cold = append(cold, hrSince(t0))
		t0 = hrNow()
		if _, _, err := s.ReadTile(ctx, x.ds, x.tile, at, false); err != nil {
			t.Fatal(err)
		}
		warm = append(warm, hrSince(t0))
		t0 = hrNow()
		if _, err := s.VersionAt(ctx, x.ds, x.tile, at); err != nil {
			t.Fatal(err)
		}
		versionAt = append(versionAt, hrSince(t0))
		s.DropCaches()
		t0 = hrNow()
		if _, _, err := s.ReadTile(ctx, x.ds, x.tile, x.info.LastT, false); err != nil {
			t.Fatal(err)
		}
		latest = append(latest, hrSince(t0))
	}
	rep.Reads = map[string]latRep{
		"cold_random_t": latencies(cold),
		"warm_random_t": latencies(warm),
		"cold_latest":   latencies(latest),
		"version_at":    latencies(versionAt),
	}
	s.Close()
	o := opt
	o.defaults()
	rep.Config = map[string]any{
		"window_ms": window, "kf_factor": o.KeyframeFactor, "max_chain": o.MaxChain,
		"level": int(o.Level), "nosync": opt.NoSync, "nowal": opt.NoWAL, "filter": filter,
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	t.Logf("%s", out)
	if p := os.Getenv("TSDB_REPLAY_OUT"); p != "" {
		os.WriteFile(p, out, 0o644)
	}
}
