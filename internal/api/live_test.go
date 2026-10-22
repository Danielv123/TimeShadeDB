package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/ingest"
)

func envLine(t testing.TB, tick int64, recv core.TimeMs, msgs ...any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": 1, "tick": tick, "src": "clusterio:1", "map": testDS.Map, "instance": 1, "recv_ms": recv, "msgs": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func chunkMsg(cx, cy int, v uint16) map[string]any {
	return map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": cx, "y": cy, "d": core.EncodeChart(chart(v))}
}

func TestIngestHTTP(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	now := f.clock()
	body := envLine(t, 60, now, chunkMsg(1, 1, 0x4444),
		map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "1.2.v9", "k": "v", "f": "player", "name": "car", "x": 3, "y": 4, "h": 0.5}}},
		map[string]any{"type": "map", "info": map[string]any{"key": testDS.Map, "title": "Renamed", "instances": []any{}}},
	) + "garbage\n"
	r := f.post("/api/v3/ingest", body)
	r.wantStatus(t, 200, "ingest")
	r.wantCache(t, "no-store", "ingest")
	var ir core.IngestResponse
	r.json(t, &ir)
	if ir.Envelopes != 1 || ir.BadLines != 1 || ir.Chunks.Accepted != 1 || ir.Samples.Accepted != 1 {
		t.Fatalf("ingest %+v", ir)
	}
	// The catalog shows the new title at once (map messages invalidate it).
	var cat core.Catalog
	f.get("/api/v3/catalog").json(t, &cat)
	if cat.Maps[0].Title != "Renamed" {
		t.Fatalf("title %q", cat.Maps[0].Title)
	}
	// gzip and zstd bodies.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(envLine(t, 120, now+1000, chunkMsg(2, 1, 0x5555))))
	zw.Close()
	r = f.post("/api/v3/ingest", gz.String(), "Content-Encoding", "gzip")
	r.json(t, &ir)
	if ir.Chunks.Accepted != 1 {
		t.Fatalf("gzip ingest %+v", ir)
	}
	enc, _ := zstd.NewWriter(nil)
	zb := enc.EncodeAll([]byte(envLine(t, 180, now+2000, chunkMsg(3, 1, 0x6666))), nil)
	r = f.post("/api/v3/ingest", string(zb), "Content-Encoding", "zstd")
	r.json(t, &ir)
	if ir.Chunks.Accepted != 1 {
		t.Fatalf("zstd ingest %+v", ir)
	}
	f.post("/api/v3/ingest", "x", "Content-Encoding", "br").wantStatus(t, 415, "unknown encoding")
	// Live content is visible at once; a commit makes it a version.
	var vr core.ViewResponse
	f.get("/api/v3/view/"+dsID+"/0?bbox=0,0,0,0").json(t, &vr)
	if len(vr.Tiles) != 1 || !vr.Tiles[0].Live || vr.Tiles[0].LiveSeq == 0 || vr.Tiles[0].Version != 2 {
		t.Fatalf("live view %+v", vr)
	}
	f.now.Store(int64(now + 2*core.Minute))
	f.srv.Step(context.Background())
	f.get("/api/v3/view/"+dsID+"/0?bbox=0,0,0,0").json(t, &vr)
	if len(vr.Tiles) != 1 || vr.Tiles[0].Live || vr.Tiles[0].Version != 3 {
		t.Fatalf("committed view %+v", vr)
	}
}

func TestIngestLimits(t *testing.T) {
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.MaxBody = 4096 }})
	big := envLine(t, 60, f.clock(), chunkMsg(1, 1, 1)) + strings.Repeat(" ", 5000) + "\n"
	f.post("/api/v3/ingest", big).wantError(t, 413, "too_large", "body over the cap")
	// A compressed body over the cap after decompression.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(strings.Repeat("\n", 100000)))
	zw.Close()
	f.post("/api/v3/ingest", gz.String(), "Content-Encoding", "gzip").wantError(t, 413, "too_large", "decompressed body over the cap")
}

// overloaded refuses every terrain batch.
type overloaded struct{ core.TerrainStore }

func (overloaded) Apply(context.Context, []core.ChunkUpdate) (core.ApplyResult, error) {
	return core.ApplyResult{}, fmt.Errorf("test: %w", core.ErrOverloaded)
}

func TestIngestBackpressure(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	f.srv.SetIngester(ingest.New(ingest.Stores{Terrain: overloaded{f.ter}, Movers: f.mov, Overlays: f.ovl}, ingest.Options{Clock: f.clock}))
	r := f.post("/api/v3/ingest", envLine(t, 60, f.clock(), chunkMsg(1, 1, 1)))
	r.wantError(t, 503, "overloaded", "overloaded terrain")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
}

// sse reads events from a live stream.
type sse struct {
	res    *http.Response
	events chan [2]string
}

func openSSE(t *testing.T, url string) *sse {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("live: %d %v", res.StatusCode, res.Header)
	}
	s := &sse{res: res, events: make(chan [2]string, 100)}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = line[7:]
			case strings.HasPrefix(line, "data: "):
				s.events <- [2]string{ev, line[6:]}
			}
		}
	}()
	t.Cleanup(func() { res.Body.Close() })
	return s
}

// next returns the next event named one of names, skipping others.
func (s *sse) next(t *testing.T, names ...string) (string, map[string]any) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-s.events:
			if !ok {
				t.Fatalf("stream ended waiting for %v", names)
			}
			if slices.Contains(names, e[0]) {
				var m map[string]any
				if err := json.Unmarshal([]byte(e[1]), &m); err != nil {
					t.Fatal(err)
				}
				return e[0], m
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %v", names)
		}
	}
}

func TestLiveEvents(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	ctx := context.Background()
	s := openSSE(t, f.ts.URL+"/api/v3/live?ds="+dsID+"&sfc="+sfcID)
	_, hello := s.next(t, "hello")
	hz := hello["horizon"].(map[string]any)
	if hello["now"].(float64) != float64(f.clock()) || hz[dsID] == nil || hz[sfcID] == nil {
		t.Fatalf("hello %v", hello)
	}
	now := f.clock()
	f.post("/api/v3/ingest", envLine(t, 60, now, chunkMsg(0, 0, 0x7777),
		map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "1.2.v9", "k": "v", "f": "player", "name": "car", "x": 3, "y": 4, "h": 0.5,
			"car": [][3]float64{{3, 4, 0.5}}}}},
	)).wantStatus(t, 200, "ingest")
	_, mv := s.next(t, "movers")
	rows := mv["rows"].([]any)
	if mv["sfc"] != sfcID || len(rows) != 1 || len(mv["movers"].([]any)) != 1 {
		t.Fatalf("movers event %v", mv)
	}
	row := rows[0].([]any)
	if row[0].(float64) != 0 || row[1].(float64) != float64(now) || row[2].(float64) != 60 || row[3].(float64) != 3 || row[5].(float64) != 0.5 || row[7] == nil {
		t.Fatalf("row %v", row)
	}
	// A sample older than the mover horizon is late: the store drops it, and so does the
	// event.
	f.post("/api/v3/ingest", envLine(t, 120, now+1000, map[string]any{"type": "move", "s": "nauvis", "m": []any{
		map[string]any{"j": "1.2.v8", "k": "v", "f": "player", "name": "car", "x": 1, "y": 1, "h": 0.5, "tick": 120 - 60*3600},
		map[string]any{"j": "1.2.v9", "k": "v", "f": "player", "name": "car", "x": 5, "y": 4, "h": 0.5},
	}})).wantStatus(t, 200, "ingest late sample")
	_, mv = s.next(t, "movers")
	if rows := mv["rows"].([]any); len(rows) != 1 || rows[0].([]any)[3].(float64) != 5 {
		t.Fatalf("movers event with a late row: %v", mv)
	}
	f.srv.Step(ctx)
	_, te := s.next(t, "tile")
	if te["ds"] != dsID || te["z"].(float64) != 0 || te["x"].(float64) != 0 || te["live"] != true || te["ls"] == nil || te["v"].(float64) != 2 {
		t.Fatalf("tile event %v", te)
	}
	// After the window closes: a horizon event and the new version.
	f.now.Store(int64(now + 2*core.Minute))
	f.srv.Step(ctx)
	sawHorizon, sawTile := false, false
	for !(sawHorizon && sawTile) {
		name, ev := s.next(t, "tile", "horizon")
		switch {
		case name == "horizon" && ev["ds"] == dsID:
			sawHorizon = true
		case name == "tile" && ev["z"].(float64) == 0 && ev["x"].(float64) == 0:
			if ev["v"].(float64) != 3 || ev["live"] != nil {
				t.Fatalf("committed tile event %v", ev)
			}
			sawTile = true
		}
	}
	// A new datastore announces a catalog change.
	f.post("/api/v3/ingest", envLine(t, 120, now+2*core.Minute, map[string]any{"type": "chunk", "s": "gleba", "f": "player", "x": 0, "y": 0})).wantStatus(t, 200, "ingest gleba")
	f.srv.Step(ctx)
	s.next(t, "catalog")
}

// TestLiveFirstScanSilent: a client connected before the server's first commit step is
// not sent an event for every existing tile.
func TestLiveFirstScanSilent(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	f.srv.live = newLiveState() // as after a restart: nothing published yet
	s := openSSE(t, f.ts.URL+"/api/v3/live?ds="+dsID)
	s.next(t, "hello")
	f.srv.Step(context.Background())
	f.srv.Step(context.Background())
	f.srv.hub.publish("", "", event("marker", map[string]any{}))
	name, _ := s.next(t, "tile", "marker")
	if name != "marker" {
		t.Fatal("the first scan published tile events")
	}
}

func TestLiveSlowClientDropped(t *testing.T) {
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.LiveBuffer = 1024 }})
	s := openSSE(t, f.ts.URL+"/api/v3/live?ds="+dsID)
	s.next(t, "hello")
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.hub.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.srv.hub.publish(dsID, "", event("tile", map[string]string{"pad": strings.Repeat("x", 2048)}))
	for range s.events {
		// The server closes the stream; the pad event is never delivered whole.
	}
	if f.srv.hub.dropped.Load() != 1 {
		t.Fatalf("dropped %d", f.srv.hub.dropped.Load())
	}
	for f.srv.hub.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.srv.hub.count() != 0 {
		t.Fatal("dropped client still registered")
	}
}

func TestHubBounds(t *testing.T) {
	h := newHub(2, 100)
	a := &client{ds: map[string]bool{"a": true}, wake: make(chan struct{}, 1)}
	b := &client{ds: map[string]bool{"b": true}, wake: make(chan struct{}, 1)}
	if !h.add(a) || !h.add(b) || h.add(&client{wake: make(chan struct{}, 1)}) {
		t.Fatal("client bound")
	}
	for range 3 {
		h.publish("a", "", make([]byte, 30))
	}
	if q, dropped := a.take(); len(q) != 3 || dropped {
		t.Fatalf("queue %d dropped %v", len(q), dropped)
	}
	h.publish("a", "", make([]byte, 101))
	if _, dropped := a.take(); !dropped || h.dropped.Load() != 1 {
		t.Fatal("over-buffer client not dropped")
	}
	if q, _ := b.take(); len(q) != 0 {
		t.Fatal("event delivered to the wrong topic")
	}
}

// discardWriter is a minimal ResponseWriter for in-process load.
type discardWriter struct {
	h      http.Header
	status int
}

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(s int)           { w.status = s }

// TestHostileTileRequests sends random tile URLs (unknown and malformed datastores,
// coordinates inside and outside the grid, versions that do not exist) and checks that
// they leave less than 2 MB behind (the Hostile requests targets in bench/targets.json) and
// answer quickly.
// hostilePath returns a random request path: mostly invalid ids, levels, coordinates and
// versions (valid false), or valid ids and levels with coordinates, versions and dates
// near the fixture's data, so that many requests find real tiles.
func hostilePath(rng *rand.Rand, valid bool) string {
	ids := []string{dsID, dsID, "gw-x.nauvis.player", "nope", "gw-test~2E1.nauvis.enemy", "A.B.C", "x~zz.y.z"}
	id := ids[rng.IntN(len(ids))]
	z := -rng.IntN(13)
	x := rng.IntN(10000) - 5000
	y := rng.IntN(10000) - 5000
	if rng.IntN(2) == 0 {
		x, y = rng.IntN(9)-4, rng.IntN(9)-4
	}
	v := rng.IntN(1000)
	if valid {
		id = dsID
		x, y = rng.IntN(7)-3, rng.IntN(3)-1
		v = rng.IntN(5)
	}
	switch rng.IntN(7) {
	case 0, 1:
		return fmt.Sprintf("/api/v3/tile/%s/%d/%d/%d/%d.bin", id, z, x, y, v)
	case 2:
		return fmt.Sprintf("/api/v3/tile/%s/%d/%d/%d/%d.png", id, z, x, y, v)
	case 3:
		return fmt.Sprintf("/api/v3/tile/%s/%d/%d/%d/live.bin", id, z, x, y)
	case 4:
		return fmt.Sprintf("/api/v3/tiledelta/%s/%d/%d/%d-%d.bin", id, x, y, v, v+rng.IntN(5))
	case 5:
		return fmt.Sprintf("/api/v3/timeline/%s/%d/2026-10-%02d.json", id, z, 10+rng.IntN(10))
	default:
		return fmt.Sprintf("/api/v3/view/%s/%d?bbox=%d,%d,%d,%d&t=%d", id, z, x, y, x+rng.IntN(70), y+rng.IntN(70), int64(day0)+rng.Int64N(int64(2*core.Day)))
	}
}

// TestHostileTileRequests answers a flood of requests with a fresh random path each
// (mostly invalid ones, then valid ones near the data) and checks that the heap does not
// grow and that each answer is quick. The fixture holds only a few tiles and versions,
// so after the warm-up the valid requests that find data are answered from the caches;
// a flood of distinct valid tiles on a large database is bounded by the cache budgets
// instead, which this test does not measure.
func TestHostileTileRequests(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	n := 1_000_000
	if testing.Short() {
		n = 100_000
	}
	run := func(rng *rand.Rand, count int, valid bool) (time.Duration, map[int]int) {
		codes := map[int]int{}
		var worst time.Duration
		w := &discardWriter{h: http.Header{}}
		for range count {
			req := httptest.NewRequest(http.MethodGet, hostilePath(rng, valid), nil)
			clear(w.h)
			w.status = 200
			t0 := time.Now()
			f.srv.ServeHTTP(w, req)
			if d := time.Since(t0); d > worst {
				worst = d
			}
			codes[w.status]++
		}
		return worst, codes
	}
	for _, valid := range []bool{false, true} {
		count := n
		if valid {
			count = n / 5
		}
		rng := rand.New(rand.NewPCG(1, uint64(len(fmt.Sprint(valid)))))
		run(rng, 20_000, valid) // warm caches, metrics and the encoder pools
		var before, after runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&before)
		t0 := time.Now()
		worst, codes := run(rng, count, valid)
		elapsed := time.Since(t0)
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&after)
		grew := int64(after.HeapInuse) - int64(before.HeapInuse)
		t.Logf("valid=%v: %d requests in %v (%.1f µs each, worst %v), codes %v, heap in use %+d bytes", valid, count, elapsed, float64(elapsed.Microseconds())/float64(count), worst, codes, grew)
		if valid && codes[200] < count/20 {
			t.Fatalf("only %d of %d valid requests found data", codes[200], count)
		}
		if grew > 2<<20 {
			t.Fatalf("heap grew %d bytes over %d requests (valid=%v)", grew, count, valid)
		}
		if per := elapsed / time.Duration(count); per > time.Millisecond {
			t.Fatalf("mean %v per request", per)
		}
	}
}

// TestConcurrentServing reads every route while ingest, commits and live clients run
// (meant for -race).
func TestConcurrentServing(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := openSSE(t, f.ts.URL+"/api/v3/live?ds="+dsID+"&sfc="+sfcID)
	go func() {
		for range s.events {
		}
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			now := f.clock() + core.TimeMs(i)*500
			f.now.Store(int64(now))
			f.in.Ingest(ctx, strings.NewReader(envLine(t, int64(60+30*i), now, chunkMsg(i%40, i%7, uint16(i)),
				map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "1.2.v9", "k": "v", "f": "player", "name": "car", "x": i, "y": 4, "h": 0.5}}})))
			f.srv.Step(ctx)
		}
	}()
	paths := []string{"/api/v3/catalog", "/api/v3/view/" + dsID + "/0?bbox=-1,-1,3,3", "/api/v3/view/" + dsID + "/-1?bbox=-2,-2,2,2",
		"/api/v3/tile/" + dsID + "/0/0/0/live.bin", "/api/v3/tile/" + dsID + "/-1/0/0/live.bin", "/api/v3/tile/" + dsID + "/0/0/0/1.bin",
		"/api/v3/movers/" + sfcID + "/live.bin", "/api/v3/timeline/" + dsID + "/0/2026-10-16.json", "/metrics"}
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &http.Client{}
			for i := w; ctx.Err() == nil; i++ {
				res, err := c.Get(f.ts.URL + paths[i%len(paths)])
				if err != nil {
					continue
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode >= 500 {
					t.Errorf("%s: %d", paths[i%len(paths)], res.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
}
