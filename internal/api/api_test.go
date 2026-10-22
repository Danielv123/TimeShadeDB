package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/wire"
)

var dsID = testDS.ID()   // gw-test~2E1.nauvis.player
var sfcID = testSfc.ID() // gw-test~2E1.nauvis

func decodeBody(t *testing.T, r resp) []byte {
	t.Helper()
	switch r.header.Get("Content-Encoding") {
	case "":
		return r.body
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return b
	case "zstd":
		d, _ := zstd.NewReader(nil)
		defer d.Close()
		b, err := d.DecodeAll(r.body, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("encoding %q", r.header.Get("Content-Encoding"))
	return nil
}

func TestCatalog(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	for _, path := range []string{"/api/v3/catalog", "/api/v3/catalog.json"} {
		r := f.get(path)
		r.wantStatus(t, 200, path)
		r.wantCache(t, ccShort, path)
		if r.header.Get("X-TSDB-Now") != fmt.Sprint(int64(f.clock())) {
			t.Fatalf("X-TSDB-Now %q", r.header.Get("X-TSDB-Now"))
		}
		var cat core.Catalog
		r.json(t, &cat)
		if cat.Version != core.APIVersion || cat.Static || cat.Now != f.clock() || len(cat.Maps) != 1 {
			t.Fatalf("catalog %+v", cat)
		}
		m := cat.Maps[0]
		if m.Key != testDS.Map || m.Title != "Test map" || len(m.Instances) != 1 || len(m.Surfaces) != 1 {
			t.Fatalf("map %+v", m)
		}
		s := m.Surfaces[0]
		if s.ID != sfcID || s.Name != "nauvis" || len(s.Edges) != 1 || len(s.Datastores) != 1 || s.Movers == nil {
			t.Fatalf("surface %+v", s)
		}
		d := s.Datastores[0]
		if d.ID != dsID || d.Force != "player" || d.WindowMs != core.DefaultTerrainWindow || d.Horizon == 0 || d.PaletteVersion != 4 {
			t.Fatalf("datastore %+v", d)
		}
		if len(d.Levels) < 2 || d.Levels[0] != 0 || d.Levels[1] != -1 {
			t.Fatalf("levels %v", d.Levels)
		}
		if d.Bounds != [4]int32{0, 0, 2, 0} {
			t.Fatalf("bounds %v", d.Bounds)
		}
		if s.Movers.First != day0+core.Hour || len(s.Movers.Days) == 0 || s.Movers.Days[0] != "2026-10-15" {
			t.Fatalf("movers %+v", s.Movers)
		}
		if cat.Icons != nil {
			t.Fatal("icons without an icons dir")
		}
		etag := r.header.Get("ETag")
		if etag == "" {
			t.Fatal("no ETag")
		}
		nm := f.get(path, "If-None-Match", etag)
		nm.wantStatus(t, http.StatusNotModified, "revalidation")
	}
}

func TestTiles(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	pal := f.get("/api/v3/palette/" + dsID + "/4.bin")
	pal.wantStatus(t, 200, "palette")
	pal.wantCache(t, ccImmutable, "palette")
	p, err := wire.DecodePalette(pal.body)
	if err != nil || len(p.Colors) != 4 {
		t.Fatalf("palette %v %v", p, err)
	}
	f.get("/api/v3/palette/"+dsID+"/5.bin").wantError(t, 404, "not_found", "palette past the end")

	// view at a time between the two versions of tile (0,0): immutable.
	v := f.get(fmt.Sprintf("/api/v3/view/%s/0?bbox=-1,-1,3,1&t=%d", dsID, day0+core.Hour+90*core.Second))
	v.wantStatus(t, 200, "view")
	v.wantCache(t, ccImmutable, "historical view")
	var vr core.ViewResponse
	v.json(t, &vr)
	if len(vr.Tiles) != 2 || vr.Tiles[0].X != 0 || vr.Tiles[0].Version != 1 || vr.Tiles[1].X != 2 || vr.Tiles[1].Version != 1 {
		t.Fatalf("view %+v", vr)
	}
	// latest: live cache class, newest versions.
	v = f.get("/api/v3/view/" + dsID + "/0?bbox=-1,-1,3,1&t=latest")
	v.wantCache(t, ccLive, "latest view")
	v.json(t, &vr)
	if len(vr.Tiles) != 2 || vr.Tiles[0].Version != 2 || vr.T != f.clock() {
		t.Fatalf("latest view %+v", vr)
	}
	// Before any data: no tiles.
	v = f.get(fmt.Sprintf("/api/v3/view/%s/0?bbox=-1,-1,3,1&t=%d", dsID, day0))
	v.json(t, &vr)
	if len(vr.Tiles) != 0 {
		t.Fatalf("early view %+v", vr)
	}
	// Coarse view.
	v = f.get("/api/v3/view/" + dsID + "/-1?bbox=-4,-4,4,4")
	v.json(t, &vr)
	if len(vr.Tiles) != 2 || vr.Tiles[0].Version != 1 {
		t.Fatalf("coarse view %+v", vr)
	}
	f.get("/api/v3/view/"+dsID+"/0?bbox=0,0,64,64").wantError(t, 400, "bad_request", "view over 4096 tiles")
	f.get("/api/v3/view/"+dsID+"/0?bbox=0,0,1").wantError(t, 400, "bad_request", "short bbox")
	f.get("/api/v3/view/"+dsID+"/1?bbox=0,0,1,1").wantError(t, 400, "bad_tile", "z > 0")
	f.get("/api/v3/view/gw-other.nauvis.player/0?bbox=0,0,1,1").wantError(t, 404, "not_found", "unknown datastore")

	// Tiles in every encoding.
	for _, enc := range []string{"identity", "gzip", "zstd"} {
		r := f.get("/api/v3/tile/"+dsID+"/0/0/0/2.bin", "Accept-Encoding", enc)
		r.wantStatus(t, 200, "tile "+enc)
		r.wantCache(t, ccImmutable, "tile")
		if got := r.header.Get("Content-Encoding"); (enc == "identity" && got != "") || (enc != "identity" && got != enc) {
			t.Fatalf("Content-Encoding %q for %s", got, enc)
		}
		if r.header.Get("Vary") != "Accept-Encoding" {
			t.Fatal("no Vary")
		}
		h, im, err := wire.DecodeTile(decodeBody(t, r), &p)
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != 2 || h.Live || im.Block(func() uint8 { _, b := (core.ChunkCoord{}).Tile(); return b }())[0] != 0x3333 {
			t.Fatalf("tile header %+v", h)
		}
	}
	r := f.get("/api/v3/tile/" + dsID + "/0/0/0/1.bin")
	if _, im, err := wire.DecodeTile(r.body, &p); err != nil || im.Pix[core.TilePixelIndex(func() uint8 { _, b := (core.ChunkCoord{}).Tile(); return b }(), 0)] != 0x1111 {
		t.Fatalf("version 1: %v", err)
	}
	f.get("/api/v3/tile/"+dsID+"/0/0/0/3.bin").wantError(t, 404, "not_found", "version past the latest")
	f.get("/api/v3/tile/"+dsID+"/0/5/5/1.bin").wantError(t, 404, "not_found", "tile without data")
	f.get("/api/v3/tile/"+dsID+"/0/99999/0/1.bin").wantError(t, 400, "bad_tile", "tile outside the grid")
	f.get("/api/v3/tile/"+dsID+"/-11/0/0/1.bin").wantError(t, 400, "bad_tile", "level below MinZ")
	f.get("/api/v3/tile/"+dsID+"/0/a/0/1.bin").wantError(t, 400, "bad_tile", "non-numeric x")
	f.get("/api/v3/tile/gw-test~2e1.nauvis.player/0/0/0/1.bin").wantError(t, 400, "bad_request", "lower-case escape")
	f.get("/api/v3/tile/"+dsID+"/0/0/0/1.gif").wantError(t, 404, "not_found", "unknown suffix")

	// Live tiles: never cached, live flag set.
	r = f.get("/api/v3/tile/" + dsID + "/0/0/0/live.bin")
	r.wantStatus(t, 200, "live tile")
	r.wantCache(t, "no-store", "live tile")
	if h, _, err := wire.DecodeTile(r.body, &p); err != nil || !h.Live || h.Version != 2 {
		t.Fatalf("live tile %+v %v", h, err)
	}
	r = f.get("/api/v3/tile/" + dsID + "/-1/0/0/live.bin")
	r.wantStatus(t, 200, "coarse live tile")
	if h, _, err := wire.DecodeTile(r.body, nil); err != nil || !h.Live || h.Format != wire.FormatRGB565 {
		t.Fatalf("coarse live tile %+v %v", h, err)
	}
	r = f.get("/api/v3/tile/" + dsID + "/-1/0/0/1.bin")
	r.wantStatus(t, 200, "coarse tile")
	if h, _, err := wire.DecodeTile(r.body, nil); err != nil || h.Z != -1 || h.Version != 1 {
		t.Fatalf("coarse tile %+v %v", h, err)
	}
	r = f.get("/api/v3/tile/" + dsID + "/0/0/0/2.png")
	r.wantStatus(t, 200, "png")
	if r.header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(r.body, []byte("\x89PNG")) {
		t.Fatal("png body")
	}

	// Deltas.
	r = f.get("/api/v3/tiledelta/" + dsID + "/0/0/1-2.bin")
	r.wantStatus(t, 200, "delta")
	r.wantCache(t, ccImmutable, "delta")
	dh, deltas, err := wire.DecodeTileDelta(r.body, &p)
	if err != nil || dh.From != 1 || dh.To != 2 || len(deltas) != 1 || deltas[0].Pixels[0] != 0x3333 {
		t.Fatalf("delta %+v %v %v", dh, deltas, err)
	}
	f.get("/api/v3/tiledelta/"+dsID+"/0/0/2-1.bin").wantError(t, 400, "bad_request", "reversed delta")
	f.get("/api/v3/tiledelta/"+dsID+"/0/0/2-9.bin").wantError(t, 404, "not_found", "delta past the latest")
}

func TestTimeline(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	r := f.get("/api/v3/timeline/" + dsID + "/0/2026-10-15.json")
	r.wantStatus(t, 200, "timeline")
	r.wantCache(t, ccImmutable, "final timeline")
	var tl core.TimelineResponse
	r.json(t, &tl)
	if !tl.Final || tl.Start != day0 || len(tl.Tiles) != 2 {
		t.Fatalf("timeline %+v", tl)
	}
	t0 := tl.Tiles[0]
	if t0.X != 0 || t0.V0 != 0 || len(t0.Points) != 2 || t0.Points[0] != [2]int64{int64(day0 + core.Hour + core.Minute), 1} || t0.Points[1][1] != 2 {
		t.Fatalf("tile timeline %+v", t0)
	}
	// The next day: v0 carries the versions, not final (the horizon is in that day).
	r = f.get("/api/v3/timeline/" + dsID + "/0/2026-10-16.json")
	r.wantCache(t, ccShort, "open timeline")
	r.json(t, &tl)
	if tl.Final || len(tl.Tiles) != 2 || tl.Tiles[0].V0 != 2 || len(tl.Tiles[0].Points) != 0 {
		t.Fatalf("next day %+v", tl)
	}
	if r.header.Get("ETag") == "" {
		t.Fatal("open timeline without ETag")
	}
	// Coarse: the pyramid built its first version at the horizon (the next day).
	r = f.get("/api/v3/timeline/" + dsID + "/-1/2026-10-15.json")
	r.json(t, &tl)
	if !tl.Final || len(tl.Tiles) != 0 {
		t.Fatalf("coarse day0 %+v", tl)
	}
	f.get("/api/v3/timeline/"+dsID+"/0/2026-13-01.json").wantError(t, 400, "bad_request", "bad date")
	f.get("/api/v3/timeline/"+dsID+"/0/2026-10-15").wantError(t, 404, "not_found", "no suffix")
}

func TestMovers(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	r := f.get("/api/v3/movers/" + sfcID + "/day/2026-10-15.json")
	r.wantStatus(t, 200, "day")
	r.wantCache(t, ccImmutable, "final day")
	var day core.DayIndex
	r.json(t, &day)
	if !day.Final || len(day.Windows) != 2 || len(day.Keyframes) == 0 {
		t.Fatalf("day %+v", day)
	}
	start := day.Windows[0].Start
	// zstd clients get the stored frame; others a decoded and recompressed body.
	for _, enc := range []string{"zstd", "gzip", "identity"} {
		w := f.get(fmt.Sprintf("/api/v3/movers/%s/window/%d.bin", sfcID, start), "Accept-Encoding", enc)
		w.wantStatus(t, 200, "window "+enc)
		w.wantCache(t, ccImmutable, "window")
		mw, err := wire.DecodeMoverWindow(decodeBody(t, w), testSfc)
		if err != nil || mw.Kind != core.WindowClosed || len(mw.Rows) != 300 || mw.Movers[0].ID != "1.1.7" {
			t.Fatalf("window %s: %v", enc, err)
		}
	}
	kf := day.Keyframes[len(day.Keyframes)-1]
	k := f.get(fmt.Sprintf("/api/v3/movers/%s/keyframe/%d.bin", sfcID, kf), "Accept-Encoding", "gzip")
	k.wantStatus(t, 200, "keyframe")
	if mw, err := wire.DecodeMoverWindow(decodeBody(t, k), testSfc); err != nil || mw.Kind != core.WindowKeyframe || len(mw.Rows) != 1 {
		t.Fatalf("keyframe %v", err)
	}
	l := f.get("/api/v3/movers/" + sfcID + "/live.bin")
	l.wantStatus(t, 200, "live")
	l.wantCache(t, ccLive, "live movers")
	if l.header.Get("X-TSDB-Now") == "" {
		t.Fatal("live without X-TSDB-Now")
	}
	if mw, err := wire.DecodeMoverWindow(l.body, testSfc); err != nil || mw.Kind != core.WindowLive || mw.Start != f.clock() {
		t.Fatalf("live %v", err)
	}
	f.get(fmt.Sprintf("/api/v3/movers/%s/window/%d.bin", sfcID, start+7)).wantError(t, 404, "not_found", "no window")
	f.get("/api/v3/movers/"+sfcID+"/day/2026-10-01.json").wantError(t, 404, "not_found", "empty day")
	f.get("/api/v3/movers/"+sfcID+"/nope/1.bin").wantError(t, 404, "not_found", "unknown kind")
	f.get("/api/v3/movers/bad id/live.bin").wantError(t, 400, "bad_request", "bad surface id")
}

func TestOverlays(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	r := f.get("/api/v3/overlays/" + dsID + "/0/0.json")
	r.wantStatus(t, 200, "overlays")
	r.wantCache(t, ccShort, "overlays")
	var or core.OverlayResponse
	r.json(t, &or)
	if or.Version == 0 || len(or.Intervals) != 1 || or.Intervals[0].Text != "coal" {
		t.Fatalf("overlays %+v", or)
	}
	etag := r.header.Get("ETag")
	if etag != fmt.Sprintf(`"%d"`, or.Version) {
		t.Fatalf("ETag %q", etag)
	}
	f.get("/api/v3/overlays/"+dsID+"/0/0.json", "If-None-Match", etag).wantStatus(t, 304, "overlay revalidation")
	r = f.get(fmt.Sprintf("/api/v3/overlays/%s/0/0.json?t0=%d&t1=%d", dsID, day0, day0+core.Hour))
	r.json(t, &or)
	if len(or.Intervals) != 0 {
		t.Fatalf("range before the tag: %+v", or)
	}
	r = f.get("/api/v3/overlays/" + dsID + "/0/0.json?kinds=recipe")
	r.json(t, &or)
	if len(or.Intervals) != 0 {
		t.Fatalf("kind filter: %+v", or)
	}
	f.get("/api/v3/overlays/"+dsID+"/0/0.json?kinds=nope").wantError(t, 400, "bad_request", "bad kind")
	r = f.get("/api/v3/overlays/gw-other.nauvis.player/0/0.json")
	r.wantStatus(t, 200, "unknown datastore overlays")
}

func TestAuth(t *testing.T) {
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.ReadToken = "rt"; c.IngestToken = "it" }})
	f.get("/api/v3/catalog").wantError(t, 401, "unauthorized", "no read token")
	f.get("/api/v3/catalog", "Authorization", "Bearer nope").wantError(t, 401, "unauthorized", "wrong read token")
	r := f.get("/api/v3/catalog", "Authorization", "Bearer rt")
	r.wantStatus(t, 200, "read token")
	r.wantCache(t, "private, max-age=10", "private cache")
	f.get("/api/v3/catalog?token=rt").wantStatus(t, 200, "query token")
	f.get("/metrics").wantError(t, 401, "unauthorized", "metrics without token")
	f.get("/healthz").wantStatus(t, 200, "healthz is open")
	f.get("/").wantStatus(t, 200, "viewer is open")
	f.post("/api/v3/ingest", "").wantError(t, 401, "unauthorized", "no ingest token")
	f.post("/api/v3/ingest", "", "Authorization", "Bearer rt").wantError(t, 401, "unauthorized", "read token on ingest")
	f.post("/api/v3/ingest", "", "Authorization", "Bearer it").wantStatus(t, 200, "ingest token")
	f.get("/api/v3/ingest", "Authorization", "Bearer it").wantStatus(t, 405, "GET ingest")
}

func TestWebAndHealth(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	r := f.get("/", "Accept-Encoding", "gzip")
	r.wantStatus(t, 200, "index")
	r.wantCache(t, "no-cache", "index")
	if r.header.Get("Content-Encoding") != "gzip" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("index headers %v", r.header)
	}
	r = f.get("/assets/app-1.js")
	r.wantCache(t, ccImmutable, "asset")
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("js type %q", r.header.Get("Content-Type"))
	}
	f.get("/assets/missing.js").wantStatus(t, 404, "missing asset")
	f.get("/../etc/passwd").wantStatus(t, 404, "traversal")
	f.get("/healthz").wantStatus(t, 200, "healthz")
	f.get("/api/v3/nope").wantError(t, 404, "not_found", "unknown route")
	m := f.get("/metrics")
	m.wantStatus(t, 200, "metrics")
	for _, name := range []string{core.MetricHTTPRequests, core.MetricCommits, core.MetricEncodedCacheBytes, core.MetricLiveClients, core.MetricChunkUpdates, "go_memstats_heap_inuse_bytes", core.MetricHTTPDuration + "_bucket"} {
		if !bytes.Contains(m.body, []byte(name)) {
			t.Fatalf("metrics lack %s:\n%s", name, m.body)
		}
	}
}
