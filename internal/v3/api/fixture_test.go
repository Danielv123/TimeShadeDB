package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/ingest"
	"timeshadedb/internal/v3/movers"
	"timeshadedb/internal/v3/overlays"
	"timeshadedb/internal/v3/pyramid"
	"timeshadedb/internal/v3/terrain"
	"timeshadedb/internal/v3/tileenc"
)

// day0 is 2026-10-15 00:00 UTC; the fixture's history lies in that day.
const day0 core.TimeMs = 1_792_022_400_000

var (
	testDS  = core.DatastoreKey{Map: "gw-test.1", Surface: "nauvis", Force: "player"}
	testSfc = core.SurfaceKey{Map: "gw-test.1", Surface: "nauvis"}
)

type fixture struct {
	t    *testing.T
	dir  string
	now  atomic.Int64
	ter  *terrain.Store
	pyr  *pyramid.Pyramid
	rend *tileenc.Renderer
	mov  *movers.Store
	ovl  *overlays.Store
	maps *ingest.Maps
	in   *ingest.Ingester
	srv  *Server
	ts   *httptest.Server
}

type fixtureOpt struct {
	cfg        func(*Config)
	openWindow int64
	moverBytes int64
}

func (f *fixture) clock() core.TimeMs { return core.TimeMs(f.now.Load()) }

func chart(v uint16) *core.ChunkPixels {
	var px core.ChunkPixels
	for i := range px {
		px[i] = v
	}
	return &px
}

// newFixture opens real stores in a temp dir with a small history in day0:
//   - terrain: chunk (0,0) of tile (0,0) changes at 01:00:10 and 01:01:10, chunk (24,0)
//     of tile (2,0) at 01:00:20; committed and built into the pyramid;
//   - movers: a train sampled every second for 10 minutes from 01:00, committed;
//   - overlays: a tag at (5, 5) opened at 01:00:30.
//
// The clock then stands at day0 + 26 h (the next day), so day0 is final.
func newFixture(t *testing.T, o fixtureOpt) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	f.now.Store(int64(day0 + core.Hour))
	ctx := context.Background()
	var err error
	topt := terrain.Options{NoSync: true, OpenWindowBytes: o.openWindow}
	if f.ter, err = terrain.Open(f.dir, topt); err != nil {
		t.Fatal(err)
	}
	if f.pyr, err = pyramid.New(ctx, f.ter, f.ter.SeriesStore(), pyramid.Options{Intervals: []core.TimeMs{1}, Lazy: true}); err != nil {
		t.Fatal(err)
	}
	if f.rend, err = tileenc.NewRenderer(f.pyr, f.ter, tileenc.Options{}); err != nil {
		t.Fatal(err)
	}
	if f.mov, err = movers.Open(f.dir, movers.Options{SyncInterval: -1, Clock: f.clock, OpenBytes: o.moverBytes, MinTime: -1, Logf: func(string, ...any) {}}); err != nil {
		t.Fatal(err)
	}
	if f.ovl, err = overlays.Open(f.dir, overlays.Options{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	if f.maps, err = ingest.OpenMaps(f.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.maps.Put(core.MapInfo{Key: testDS.Map, Title: "Test map", TileSize: 512,
		Instances: []core.InstanceInfo{{ID: "1", Name: "gridworld_0_0"}},
		Edges:     map[string][]core.Quad{"nauvis": {{1, 2, 3, 4}}}}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Terrain: f.ter, Tiles: f.pyr, Renderer: f.rend, Movers: f.mov, Overlays: f.ovl, Maps: f.maps,
		Web: fstest.MapFS{
			"index.html":        {Data: []byte("<!doctype html><title>v</title>" + strings.Repeat(" ", 600))},
			"assets/app-1.js":   {Data: []byte("console.log(1);" + strings.Repeat(" ", 600))},
			"assets/logo-1.png": {Data: []byte("\x89PNG")},
		},
		Clock:    f.clock,
		LivePing: time.Hour,
	}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	if f.srv, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	f.in = ingest.New(ingest.Stores{Terrain: f.ter, Movers: f.mov, Overlays: f.ovl, Maps: f.maps},
		ingest.Options{Clock: f.clock, Notifier: f.srv.Notifier()})
	f.srv.SetIngester(f.in)

	// History.
	h := day0 + core.Hour
	if _, err := f.ter.Apply(ctx, []core.ChunkUpdate{
		{DS: testDS, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: h + 10*core.Second, Pixels: chart(0x1111)},
		{DS: testDS, Chunk: core.ChunkCoord{X: 24, Y: 0}, T: h + 20*core.Second, Pixels: chart(0x2222)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ter.Apply(ctx, []core.ChunkUpdate{
		{DS: testDS, Chunk: core.ChunkCoord{X: 0, Y: 0}, T: h + 70*core.Second, Pixels: chart(0x3333)},
	}); err != nil {
		t.Fatal(err)
	}
	var samples []core.MoverSample
	meta := core.MoverMeta{ID: "1.1.7", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 1, Kinds: "L"}
	for i := range 600 {
		samples = append(samples, core.MoverSample{Surface: testSfc, Meta: meta, T: h + core.TimeMs(i)*core.Second, Tick: int64(i * 60), X: float64(i), Y: 3, Orientation: 0.25})
	}
	if _, err := f.mov.Append(ctx, samples); err != nil {
		t.Fatal(err)
	}
	x, y := 5.0, 5.0
	if _, err := f.ovl.Apply(ctx, []core.OverlayEvent{{DS: testDS, T: h + 30*core.Second, Interval: core.Interval{Kind: core.OverlayTag, Key: "1/1", Instance: 1, X: x, Y: y, Icon: "item/coal", Text: "coal"}}}); err != nil {
		t.Fatal(err)
	}
	f.now.Store(int64(day0 + 26*core.Hour))
	f.srv.Step(ctx)

	f.ts = httptest.NewServer(f.srv)
	t.Cleanup(func() {
		f.ts.Close()
		f.ovl.Close()
		f.mov.Close()
		f.pyr.Close()
		f.rend.Close()
		f.ter.Close()
	})
	return f
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (f *fixture) get(path string, hdr ...string) resp {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	// Disable the transport's transparent gzip so Content-Encoding is visible.
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp{res.StatusCode, res.Header, b}
}

func (f *fixture) post(path, body string, hdr ...string) resp {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	// Zero the target: json reuses slice elements, so fields absent from the body would
	// keep values from an earlier decode.
	reflect.ValueOf(v).Elem().SetZero()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) wantStatus(t *testing.T, status int, what string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d (%s)", what, r.status, status, r.body)
	}
}

func (r resp) wantError(t *testing.T, status int, code, what string) {
	t.Helper()
	r.wantStatus(t, status, what)
	var e core.ErrorResponse
	r.json(t, &e)
	if e.Code != code {
		t.Fatalf("%s: code %q, want %q", what, e.Code, code)
	}
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("%s: error Cache-Control %q", what, cc)
	}
}

const (
	ccImmutable = "public, max-age=31536000, immutable"
	ccShort     = "public, max-age=10"
	ccLive      = "public, max-age=2"
)

func (r resp) wantCache(t *testing.T, cc, what string) {
	t.Helper()
	if got := r.header.Get("Cache-Control"); got != cc {
		t.Fatalf("%s: Cache-Control %q, want %q", what, got, cc)
	}
}
