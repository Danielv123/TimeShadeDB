package benchlib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
)

// The test binary doubles as a fake server: TestMain runs it when BENCHLIB_FAKE is set.
func TestMain(m *testing.M) {
	switch os.Getenv("BENCHLIB_FAKE") {
	case "v3":
		fakeV3()
		return
	case "v2":
		fakeV2(os.Args[len(os.Args)-1])
		return
	case "fatal", "orphan":
		fakeParent(os.Getenv("BENCHLIB_FAKE"))
		return
	}
	os.Exit(m.Run())
}

var fakeIngested atomic.Int64

// fakeV3 serves a minimal subset of docs/v3/API.md on 127.0.0.1:0 and prints the
// listening line first, as the v3 contract requires.
func fakeV3() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Printf("listening on %s\n", l.Addr())
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/api/v3/catalog", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(core.Catalog{Version: 1, Maps: []core.CatalogMap{{
			MapInfo: core.MapInfo{Key: "m"},
			Surfaces: []core.CatalogSurface{{Name: "nauvis", ID: "m.nauvis", Datastores: []core.CatalogDatastore{{
				Force: "player", ID: "m.nauvis.player", First: 1000, Last: 5000, Bounds: [4]int32{-1, -1, 1, 0},
			}}}},
		}}})
	})
	mux.HandleFunc("/api/v3/view/", func(w http.ResponseWriter, r *http.Request) {
		var x0, y0, x1, y1 int32
		fmt.Sscanf(r.URL.Query().Get("bbox"), "%d,%d,%d,%d", &x0, &y0, &x1, &y1)
		vr := core.ViewResponse{DS: "m.nauvis.player"}
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				vr.Tiles = append(vr.Tiles, core.ViewTile{X: x, Y: y, TileState: core.TileState{Version: 3}})
			}
		}
		json.NewEncoder(w).Encode(vr)
	})
	mux.HandleFunc("/api/v3/tile/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "nosuch") || strings.HasSuffix(r.URL.Path, "/999.bin") {
			http.Error(w, `{"error":"x","code":"not_found"}`, 404)
			return
		}
		w.Write(make([]byte, 100))
	})
	mux.HandleFunc("/api/v3/ingest", func(w http.ResponseWriter, r *http.Request) {
		// API.md 5: 200 with counts even for bad lines and rejected messages; a line
		// over 4 MiB is a bad line; a body over 32 MiB is 413.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20+1))
		if len(body) > 32<<20 {
			w.WriteHeader(413)
			io.WriteString(w, `{"error":"too large","code":"too_large"}`)
			return
		}
		var resp core.IngestResponse
		for _, line := range bytes.Split(body, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var env core.Envelope
			if len(line) > 4<<20 || json.Unmarshal(line, &env) != nil || env.Src == "" || env.RecvMs == 0 {
				resp.BadLines++
				continue
			}
			if strings.Contains(env.Src, "busy") {
				w.WriteHeader(503)
				io.WriteString(w, `{"error":"busy","code":"overloaded"}`)
				return
			}
			reject := strings.Contains(env.Src, "reject")
			resp.Envelopes++
			for _, m := range env.Msgs {
				var h core.MsgHeader
				json.Unmarshal(m, &h)
				switch h.Type {
				case "chunk":
					var c core.ChunkMsg
					json.Unmarshal(m, &c)
					if c.D != nil {
						if _, err := core.DecodeChart(*c.D); err != nil {
							resp.Chunks.Invalid++
							continue
						}
					}
					if reject {
						resp.Chunks.Late++
					} else {
						resp.Chunks.Accepted++
					}
				case "move":
					var mv core.MoveMsg
					json.Unmarshal(m, &mv)
					resp.Samples.Accepted += len(mv.M)
				}
			}
		}
		json.NewEncoder(w).Encode(resp)
	})
	http.Serve(l, mux)
}

// fakeParent is a bench tool in miniature: it starts a fake v3 server, prints its pid,
// and then exits through Fatal ("fatal") or straight through os.Exit ("orphan", which
// skips StopAll and relies on the child being tied to this process).
func fakeParent(mode string) {
	exe, _ := os.Executable()
	os.Setenv("BENCHLIB_FAKE", "v3")
	s, err := StartServer(ServerSpec{Binary: exe, Args: []string{"-test.run=^$", "{listen}"}, Timeout: 20 * time.Second})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	fmt.Printf("pid %d\n", s.PID)
	if mode == "orphan" {
		os.Exit(3)
	}
	Fatal(fmt.Errorf("simulated tool failure"))
}

// fakeV2 serves the master routes on the address given as the last argument.
func fakeV2(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chunk/saves", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"saves":[{"savefile_uuid":"m","datastores":[{"surface":"nauvis","force":"player","latest_tick":5000,"chunk_count":3,"min_tile_x":0,"max_tile_x":1,"min_tile_y":0,"max_tile_y":0}]}]}`)
	})
	mux.HandleFunc("/api/ingest/chunk/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n := strings.Count(string(b), "\n")
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if _, err := ParseTSVRow("m", line); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		fmt.Fprintf(w, `{"accepted_rows":%d}`, n)
	})
	mux.HandleFunc("/api/chunk/tiles/", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 2000)) })
	http.ListenAndServe(addr, mux)
}

func startFake(t *testing.T, kind string) *Server {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	os.Setenv("BENCHLIB_FAKE", kind)
	defer os.Unsetenv("BENCHLIB_FAKE")
	args := []string{"-test.run=^$", "{listen}"}
	if kind == "v2" {
		args = []string{"-test.run=^$", "{addr}"}
	}
	s, err := StartServer(ServerSpec{Binary: exe, Args: args, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestStatsPercentiles(t *testing.T) {
	var xs []float64
	for i := 1; i <= 100; i++ {
		xs = append(xs, float64(i))
	}
	s := Summarize(xs)
	if s.N != 100 || s.P50 != 50 || s.P90 != 90 || s.P99 != 99 || s.Max != 100 || s.Mean != 50.5 {
		t.Fatalf("summary %+v", s)
	}
	if got := Summarize([]float64{math.NaN(), 3}); got.N != 1 || got.P50 != 3 {
		t.Fatalf("NaN handling %+v", got)
	}
	if b := LinearSlope([][2]float64{{0, 1}, {1, 3}, {2, 5}}); b != 2 {
		t.Fatalf("slope %v", b)
	}
}

func TestSliceRatesAndFlatness(t *testing.T) {
	// 10 requests of 100 rows; the last five take twice as long.
	st := SendStats{Rows: 1000}
	tm := 0.0
	for i := 1; i <= 10; i++ {
		if i <= 5 {
			tm += 1
		} else {
			tm += 2
		}
		st.Progress = append(st.Progress, [2]float64{float64(100 * i), tm})
	}
	st.Wall = time.Duration(tm * float64(time.Second))
	r := st.SliceRates(10)
	if len(r) != 10 || r[1] != 100 || r[9] != 50 {
		t.Fatalf("rates %v", r)
	}
	if f := Flatness(r); f != 0.5 {
		t.Fatalf("flatness %v", f)
	}
}

func TestTSVRoundTrip(t *testing.T) {
	px := new(core.ChunkPixels)
	for i := range px {
		px[i] = uint16(i * 37)
	}
	row := ChunkRow{T: 1791864053370, Map: "gw-x.1", Surface: "nauvis", Force: "player", CX: -19, CY: 30, Pixels: px}
	got, err := ParseTSVRow("gw-x.1", V2Line(row))
	if err != nil {
		t.Fatal(err)
	}
	if got.T != row.T || got.CX != -19 || got.CY != 30 || got.Force != "player" || *got.Pixels != *px {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if MapKeyFromStreamFile("/x/gw-bc41437d~2E1.chunks.tsv") != "gw-bc41437d.1" {
		t.Fatal("map key from file name")
	}
	if _, err := ParseTSVRow("m", "1\tnauvis\t1,2\tplayer\tabc"); err == nil {
		t.Fatal("short payload accepted")
	}
}

func TestSynthHotShare(t *testing.T) {
	cfg := DefaultSynth()
	cfg.Chunks = 5000
	s := NewSynth(cfg)
	s.NextChunks(cfg.Chunks) // initial charting
	hot := map[core.ChunkCoord]bool{}
	for _, c := range s.HotChunks() {
		hot[c] = true
	}
	if len(hot) != 20 {
		t.Fatalf("hot set %d, want 0.4%% of 5000", len(hot))
	}
	rows := s.NextChunks(20000)
	n := 0
	for _, r := range rows {
		if hot[core.ChunkCoord{X: r.CX, Y: r.CY}] {
			n++
		}
	}
	share := float64(n) / float64(len(rows))
	if share < 0.95 || share > 0.99 {
		t.Fatalf("hot share %.3f, want about 0.97", share)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].T < rows[i-1].T {
			t.Fatal("chunk times go backwards")
		}
	}
	m := s.NextMovers(100)
	if len(m) != 100 || m[0].Entry.K != "t" || m[25].T-m[0].T != cfg.TrainSampleMs {
		t.Fatalf("movers %d first %+v", len(m), m[0])
	}
}

func TestV3EncodingAnchorsTime(t *testing.T) {
	v := &V3{Src: "standalone:bench"}
	px := new(core.ChunkPixels)
	px[5] = 0xF8C3
	rows := []ChunkRow{
		{T: 1791630000000, Map: "m", Surface: "nauvis", Force: "player", CX: 1, CY: 2, Pixels: px},
		{T: 1791630012345, Map: "m", Surface: "nauvis", Force: "player", CX: 1, CY: 3, Pixels: px},
		{T: 1791630012400, Map: "m", Surface: "nauvis", Force: "player", CX: 1, CY: 4, Deleted: true},
	}
	bodies, err := v.EncodeChunks(rows)
	if err != nil || len(bodies) != 1 || bodies[0].Rows != 3 {
		t.Fatalf("bodies %v %v", bodies, err)
	}
	var env core.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(bodies[0].Data), &env); err != nil {
		t.Fatal(err)
	}
	if env.V != 1 || env.Map != "m" || env.Src == "" || env.RecvMs != 1791630012400 || env.Seq == nil {
		t.Fatalf("envelope %+v", env)
	}
	// PROTOCOL.md 4: offset = recv_ms - tick*1000/60; t_ms(tick) = offset + tick*1000/60.
	offset := float64(env.RecvMs) - float64(env.Tick)*1000/60
	for i, m := range env.Msgs {
		var c core.ChunkMsg
		if err := json.Unmarshal(m, &c); err != nil {
			t.Fatal(err)
		}
		tms := offset + float64(*c.Tick)*1000/60
		if d := math.Abs(tms - float64(rows[i].T)); d > 17 {
			t.Fatalf("message %d maps to %v, want %d (off by %v ms)", i, tms, rows[i].T, d)
		}
		if rows[i].Deleted != (c.D == nil) {
			t.Fatalf("message %d deleted mismatch", i)
		}
		if c.D != nil {
			got, err := core.DecodeChart(*c.D)
			if err != nil || *got != *px {
				t.Fatalf("chart round trip: %v", err)
			}
		}
	}
}

func TestFakeV3EndToEnd(t *testing.T) {
	s := startFake(t, "v3")
	if !strings.HasPrefix(s.BaseURL, "http://127.0.0.1:") || s.Startup <= 0 {
		t.Fatalf("server %+v", s)
	}
	if mi, err := s.Memory(); err != nil || mi.RSS == 0 {
		t.Fatalf("memory %+v %v", mi, err)
	}
	c := NewClient(s.BaseURL, 2)
	tgt, _ := NewTarget("v3", c)
	ctx := context.Background()
	synth := NewSynth(DefaultSynth())
	n := 0
	st, err := SendPipeline(ctx, c, 1, 4, PipelineOptions{}, func() ([]ChunkRow, bool) {
		if n >= 5 {
			return nil, false
		}
		n++
		return synth.NextChunks(500), true
	}, tgt.EncodeChunks)
	if err != nil || st.Rows != 2500 || st.Requests != 5 || len(st.Progress) != 5 || st.Outcome.Accepted != 2500 {
		t.Fatalf("chunks %+v %v", st, err)
	}
	m := 0
	mst, err := SendPipeline(ctx, c, 2, 2, PipelineOptions{}, func() ([]MoverRow, bool) {
		if m >= 3 {
			return nil, false
		}
		m++
		return synth.NextMovers(1000), true
	}, tgt.EncodeMovers)
	if err != nil || mst.Rows != 3000 || mst.Outcome.Accepted != 3000 {
		t.Fatalf("movers %+v %v", mst, err)
	}
	dss, err := tgt.Datastores(ctx)
	if err != nil || len(dss) != 1 || dss[0].ID != "m.nauvis.player" {
		t.Fatalf("datastores %v %v", dss, err)
	}
	refs, vd, err := tgt.Tiles(ctx, dss[0], 0, nil)
	if err != nil || len(refs) != 6 || vd <= 0 || !strings.HasSuffix(refs[0].URL, "/3.bin") {
		t.Fatalf("tiles %v %v %v", refs, vd, err)
	}
	refs, _, err = tgt.Tiles(ctx, dss[0], -1, nil)
	if err != nil || len(refs) != 4 { // x in [-1,1] -> [-1,0], y in [-1,0] -> [-1,0]
		t.Fatalf("level -1 tiles %v %v", refs, err)
	}
	r, err := c.Get(ctx, tgt.HostileURL(newRand(), nil), false, nil)
	if err != nil || r.Status != 404 {
		t.Fatalf("hostile %+v %v", r, err)
	}
}

func TestFakeV2EndToEnd(t *testing.T) {
	s := startFake(t, "v2")
	c := NewClient(s.BaseURL, 2)
	tgt, _ := NewTarget("v2", c)
	if _, err := tgt.EncodeMovers(nil); err != ErrUnsupported {
		t.Fatalf("v2 movers: %v", err)
	}
	synth := NewSynth(DefaultSynth())
	n := 0
	st, err := SendPipeline(context.Background(), c, 1, 2, PipelineOptions{}, func() ([]ChunkRow, bool) {
		if n >= 2 {
			return nil, false
		}
		n++
		return synth.NextChunks(100), true
	}, tgt.EncodeChunks)
	if err != nil || st.Rows != 200 {
		t.Fatalf("%+v %v", st, err)
	}
	dss, err := tgt.Datastores(context.Background())
	if err != nil || len(dss) != 1 || dss[0].Last != 5000 {
		t.Fatalf("%v %v", dss, err)
	}
	refs, _, _ := tgt.Tiles(context.Background(), dss[0], 0, nil)
	if len(refs) != 2 || !strings.Contains(refs[0].URL, "/m/player/nauvis/0/0/0.png?tick=5000") {
		t.Fatalf("refs %v", refs)
	}
}

func TestForbiddenPorts(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := FreePort()
		if err != nil {
			t.Fatal(err)
		}
		if forbiddenPorts[p] {
			t.Fatalf("got forbidden port %d", p)
		}
	}
}

func newRand() *rand.Rand { return rand.New(rand.NewSource(7)) }

func TestClockResolution(t *testing.T) {
	// Sub-millisecond targets need a clock much finer than Windows' 0.5 ms Go clock.
	best := time.Hour
	for i := 0; i < 100; i++ {
		a := Now()
		var d time.Duration
		for d = Since(a); d <= 0; d = Since(a) {
		}
		best = min(best, d)
	}
	if best > 50*time.Microsecond {
		t.Fatalf("clock step %v, want <= 50us", best)
	}
	a := Now()
	time.Sleep(20 * time.Millisecond)
	if d := Since(a); d < 15*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("20 ms sleep measured as %v", d)
	}
}
