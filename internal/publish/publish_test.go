package publish

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"timeshadedb/internal/core"
)

// AWS's published examples ("Signature Calculations for the Authorization Header:
// Transferring Payload in a Single Chunk", S3 API reference).
func TestSigV4AWSExamples(t *testing.T) {
	aws := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "us-east-1"}
	body := []byte("Welcome to Amazon S3.")
	if got := sha256Hex(body); got != "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" {
		t.Fatalf("sha256 %s", got)
	}
	if got := amzDate(time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)); got != "20130524T000000Z" {
		t.Fatalf("amzDate %s", got)
	}
	if got := uriEncodePath("/test$file.text"); got != "/test%24file.text" {
		t.Fatalf("uriEncodePath %s", got)
	}
	put := authorization(aws, "PUT", "/test%24file.text", map[string]string{
		"date":                 "Fri, 24 May 2013 00:00:00 GMT",
		"host":                 "examplebucket.s3.amazonaws.com",
		"x-amz-content-sha256": sha256Hex(body),
		"x-amz-date":           "20130524T000000Z",
		"x-amz-storage-class":  "REDUCED_REDUNDANCY",
	})
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class," +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if put != want {
		t.Fatalf("PUT example:\n got %s\nwant %s", put, want)
	}
	get := authorization(aws, "GET", "/test.txt", map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": sha256Hex(nil),
		"x-amz-date":           "20130524T000000Z",
	})
	want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if get != want {
		t.Fatalf("GET example:\n got %s\nwant %s", get, want)
	}
}

func TestS3RequestShape(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, err := NewS3Target(S3Config{Endpoint: "http://127.0.0.1:9000/ignored/", Bucket: "map", Prefix: "/pub/", Credentials: testCreds, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "s3:http://127.0.0.1:9000/map/pub" {
		t.Fatalf("ID %s", s.ID())
	}
	req, err := s.request(context.Background(), "tile/a~2E1.b.c/0/1/2/3.bin", []byte("x"), Meta{ContentType: "application/octet-stream", ContentEncoding: "gzip", CacheControl: CacheImmutable})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "http://127.0.0.1:9000/map/pub/tile/a~2E1.b.c/0/1/2/3.bin" || req.Host != "" && req.Host != "127.0.0.1:9000" {
		t.Fatalf("url %s host %q", req.URL, req.Host)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=cache-control;content-encoding;content-type;host;x-amz-content-sha256;x-amz-date,") {
		t.Fatalf("auth %s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Amz-Date") != "20261005T120000Z" || req.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers %v", req.Header)
	}
	vh, _ := NewS3Target(S3Config{Endpoint: "https://s3.example.com", Bucket: "map", VirtualHost: true, Credentials: testCreds})
	req, _ = vh.request(context.Background(), "catalog.json", nil, Meta{})
	if req.URL.String() != "https://map.s3.example.com/catalog.json" {
		t.Fatalf("virtual host url %s", req.URL)
	}
	for _, bad := range []S3Config{
		{Endpoint: "localhost:9000", Bucket: "b", Credentials: testCreds},
		{Endpoint: "http://h", Bucket: "", Credentials: testCreds},
		{Endpoint: "http://h", Bucket: "b"},
		{Endpoint: "http://h", Bucket: "b", Prefix: "../x", Credentials: testCreds},
	} {
		if _, err := NewS3Target(bad); err == nil {
			t.Errorf("NewS3Target(%+v) accepted", bad)
		}
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"catalog.json", "tile/gw-bc41437d~2E1.nauvis.player/-3/0/-1/12.bin", "assets/index-CMzTZr05.js"} {
		if !validKey(k) {
			t.Errorf("%q refused", k)
		}
	}
	for _, k := range []string{"", "/a", "a/", "a//b", "../a", "a/../b", "a/./b", `a\b`, "a b", "a?b", "a%2Fb"} {
		if validKey(k) {
			t.Errorf("%q accepted", k)
		}
	}
}

func newTestPublisher(t *testing.T, src Source, dst Target, stateDir string, opt Options) (*Publisher, *State) {
	t.Helper()
	st, err := OpenState(stateDir, dst.ID(), false)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Now == nil {
		if f, ok := src.(*fakeDB); ok {
			opt.Now = f.now
		} else if l, ok := src.(listerDB); ok {
			opt.Now = l.now
		}
	}
	if opt.Backoff == 0 {
		opt.Backoff = time.Millisecond
	}
	p, err := New(src, dst, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(); st.Close() })
	return p, st
}

// immutableKey says whether a published key is immutable in the fake database (its
// timelines are not final).
func immutableKey(k string) bool {
	switch {
	case strings.HasPrefix(k, "tile/"), strings.HasPrefix(k, "tiledelta/"), strings.HasPrefix(k, "palette/"), strings.HasPrefix(k, "icons/"), strings.HasPrefix(k, "assets/"):
		return true
	case strings.HasPrefix(k, "movers/") && !strings.HasSuffix(k, "/live.bin"):
		return true // windows, keyframes and the final day index
	}
	return false
}

var testWeb = fstest.MapFS{
	"index.html":           {Data: []byte(`<html><head><meta name="tsdb-source" content="" /><script type="module" src="/assets/index-abc.js"></script></head></html>`)},
	"assets/index-abc.js":  {Data: bytes.Repeat([]byte("console.log(1);\n"), 40)},
	"assets/logo-abc.png":  {Data: []byte("\x89PNG....")},
	".keep":                {Data: nil},
	"favicon.svg":          {Data: []byte("<svg/>")},
	"sub/.hidden/file.txt": {Data: []byte("no")},
}

func TestPublishOnlyChangedObjects(t *testing.T) {
	db := newFakeDB()
	s3 := newFakeS3(t)
	p, _ := newTestPublisher(t, listerDB{db}, s3.target(t), t.TempDir(), Options{Web: testWeb, LiveMs: 60000})
	ctx := context.Background()

	st, err := p.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := s3.takePuts()
	want := []string{
		"catalog.json", "favicon.svg", "index.html", "assets/index-abc.js", "assets/logo-abc.png",
		"icons/sheet.png", "icons/sheet.json", "palette/m.s.player/3.bin",
		"timeline/m.s.player/0/2026-10-12.json", "timeline/m.s.player/-1/2026-10-12.json",
		"tile/m.s.player/0/0/0/0.bin", "tile/m.s.player/0/0/0/1.bin", "tile/m.s.player/0/0/0/2.bin", "tile/m.s.player/0/0/0/3.bin",
		"tile/m.s.player/0/1/0/0.bin", "tile/m.s.player/0/1/0/1.bin", "tile/m.s.player/0/1/0/2.bin",
		"tile/m.s.player/-1/0/0/0.bin", "tile/m.s.player/-1/0/0/1.bin", "tile/m.s.player/-1/0/0/2.bin",
		"tiledelta/m.s.player/0/0/1-2.bin", "tiledelta/m.s.player/0/0/2-3.bin", "tiledelta/m.s.player/1/0/1-2.bin",
		"movers/m.s/day/2026-10-12.json", "movers/m.s/window/" + itoa(db.day0) + ".bin", "movers/m.s/window/" + itoa(db.day0+300_000) + ".bin",
		"movers/m.s/keyframe/" + itoa(db.day0+3600_000) + ".bin", "movers/m.s/live.bin",
		"overlays/m.s.player/0/0.json",
	}
	// Today (the fake clock) is the data's day, so no extra timeline.
	assertKeys(t, "first round", first, want)
	if st.Uploaded != len(want) || st.Immutable == 0 {
		t.Fatalf("stats %v", st)
	}
	// Order: every immutable object before the mutable ones, the catalog after the data,
	// index.html last.
	pos := map[string]int{}
	for i, k := range first {
		pos[k] = i
	}
	for _, k := range first {
		if immutableKey(k) && pos[k] > pos["timeline/m.s.player/0/2026-10-12.json"] {
			t.Errorf("immutable %s after a mutable timeline", k)
		}
		if k != "index.html" && pos[k] > pos["index.html"] {
			t.Errorf("%s after index.html", k)
		}
		if k != "index.html" && k != "catalog.json" && pos[k] > pos["catalog.json"] {
			t.Errorf("%s after catalog.json", k)
		}
	}
	// Headers and bodies.
	cat := s3.objects["catalog.json"]
	var c map[string]any
	if err := json.Unmarshal(gunzipIf(t, cat), &c); err != nil {
		t.Fatal(err)
	}
	if c["static"] != true || c["live_ms"] != float64(60000) || cat.header.Get("Cache-Control") != CacheCatalog {
		t.Fatalf("catalog %v %v", c, cat.header)
	}
	tile := s3.objects["tile/m.s.player/0/0/0/3.bin"]
	if tile.header.Get("Content-Encoding") != "gzip" || tile.header.Get("Cache-Control") != CacheImmutable || tile.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("tile headers %v", tile.header)
	}
	if !bytes.Equal(gunzipIf(t, tile), pad("tile/m.s.player/0/0/0/3.bin")) {
		t.Fatal("tile body")
	}
	if h := s3.objects["timeline/m.s.player/0/2026-10-12.json"].header.Get("Cache-Control"); h != CacheOpen {
		t.Fatalf("open timeline cache %s", h)
	}
	if h := s3.objects["movers/m.s/day/2026-10-12.json"].header.Get("Cache-Control"); h != CacheImmutable {
		t.Fatalf("final day cache %s", h)
	}
	if h := s3.objects["assets/logo-abc.png"].header; h.Get("Content-Encoding") != "" || h.Get("Content-Type") != "image/png" {
		t.Fatalf("png headers %v", h)
	}
	if h := s3.objects["movers/m.s/live.bin"].header.Get("Cache-Control"); h != CacheLive {
		t.Fatalf("live cache %s", h)
	}
	page := string(gunzipIf(t, s3.objects["index.html"]))
	if !strings.Contains(page, `content="static"`) || !strings.Contains(page, `src="./assets/index-abc.js"`) {
		t.Fatalf("index.html %s", page)
	}

	// Nothing changed: only the live movers are stored again, and no immutable object
	// is even read from the source.
	db.mu.Lock()
	clear(db.gets)
	db.mu.Unlock()
	st, err = p.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "unchanged round", s3.takePuts(), []string{"movers/m.s/live.bin"})
	if st.Immutable != 0 || st.Skipped == 0 {
		t.Fatalf("stats %v", st)
	}
	for path := range db.gets {
		if strings.HasPrefix(path, "tile/") || strings.HasPrefix(path, "tiledelta/") || strings.Contains(path, "/window/") {
			t.Errorf("immutable %s read again", path)
		}
	}

	// A new z0 version: its tile, its delta, the changed timeline and live movers.
	db.bump(core.TileCoord{X: 1})
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "new version", s3.takePuts(), []string{
		"tile/m.s.player/0/1/0/3.bin", "tiledelta/m.s.player/1/0/2-3.bin",
		"timeline/m.s.player/0/2026-10-12.json", "movers/m.s/live.bin",
	})

	// The day becomes final: timelines are stored once more, as immutable.
	db.mu.Lock()
	db.final = true
	db.mu.Unlock()
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "final day", s3.takePuts(), []string{
		"timeline/m.s.player/0/2026-10-12.json", "timeline/m.s.player/-1/2026-10-12.json", "movers/m.s/live.bin",
	})
	if h := s3.objects["timeline/m.s.player/0/2026-10-12.json"].header.Get("Cache-Control"); h != CacheImmutable {
		t.Fatalf("final timeline cache %s", h)
	}
	db.mu.Lock()
	clear(db.gets)
	db.mu.Unlock()
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "after final", s3.takePuts(), []string{"movers/m.s/live.bin"})
	// Stored final timelines and day indexes are not read again.
	for path := range db.gets {
		if strings.HasPrefix(path, "timeline/") || strings.Contains(path, "/day/") {
			t.Errorf("final %s read again", path)
		}
	}
}

func itoa(t core.TimeMs) string { return strconv.FormatInt(int64(t), 10) }

func gunzipIf(t *testing.T, o fakeObject) []byte {
	t.Helper()
	if o.header.Get("Content-Encoding") != "gzip" {
		return o.body
	}
	r, err := gzip.NewReader(bytes.NewReader(o.body))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	g := slices.Clone(got)
	w := slices.Clone(want)
	sort.Strings(g)
	sort.Strings(w)
	if !slices.Equal(g, w) {
		t.Fatalf("%s: stored\n  %v\nwant\n  %v", what, g, w)
	}
}

// TestPublishResumesQueue stops a round twice (a failing endpoint, then a cancelled
// context, as when the process is stopped) and checks that a new publisher on the same
// state directory resumes the queue, stores every object, never stores an immutable
// object twice and still stores the catalog only after the data.
func TestPublishResumesQueue(t *testing.T) {
	db := newFakeDB()
	s3 := newFakeS3(t)
	stateDir := t.TempDir()
	ctx := context.Background()

	// The endpoint fails every request after the 6th, beyond MaxAttempts.
	s3.fail = func(n int, key string) int {
		if n > 6 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	p1, st1 := newTestPublisher(t, listerDB{db}, s3.target(t), stateDir, Options{Workers: 1, MaxAttempts: 3})
	stats, err := p1.Round(ctx)
	if err == nil || !strings.Contains(err.Error(), "failed 3 times") {
		t.Fatalf("round 1: %v %v", err, stats)
	}
	if stats.Retries != 2 || stats.Uploaded != 6 {
		t.Fatalf("round 1 stats %v", stats)
	}
	firstPuts := s3.takePuts()
	if slices.Contains(firstPuts, "catalog.json") {
		t.Fatal("catalog stored although data objects failed")
	}
	queued, err := st1.LoadQueue()
	if err != nil || len(queued) == 0 {
		t.Fatalf("queue after a failed round: %d %v", len(queued), err)
	}
	p1.Close()
	st1.Close()

	// Restart; this time the process is stopped (context cancelled) after 4 more PUTs.
	ctx2, cancel := context.WithCancel(ctx)
	s3.fail = func(n int, key string) int {
		if n > 10 {
			cancel()
			return http.StatusServiceUnavailable
		}
		return 0
	}
	p2, st2 := newTestPublisher(t, listerDB{db}, s3.target(t), stateDir, Options{Workers: 1})
	stats, err = p2.Round(ctx2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("round 2: %v", err)
	}
	if stats.Resumed != len(queued) {
		t.Fatalf("round 2 resumed %d, queue had %d", stats.Resumed, len(queued))
	}
	secondPuts := s3.takePuts()
	p2.Close()
	st2.Close()

	// Restart with a working endpoint: the round completes.
	s3.fail = nil
	p3, st3 := newTestPublisher(t, listerDB{db}, s3.target(t), stateDir, Options{Workers: 3})
	stats, err = p3.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Resumed == 0 {
		t.Fatalf("round 3 did not resume: %v", stats)
	}
	thirdPuts := s3.takePuts()
	if q, _ := st3.LoadQueue(); len(q) != 0 {
		t.Fatalf("queue left after a complete round: %d", len(q))
	}
	all := append(append(slices.Clone(firstPuts), secondPuts...), thirdPuts...)
	seen := map[string]int{}
	for _, k := range all {
		seen[k]++
	}
	for k, n := range seen {
		if immutableKey(k) && n > 1 {
			t.Errorf("immutable %s stored %d times", k, n)
		}
	}
	// Every object of a fresh export is in the bucket.
	fresh := newFakeS3(t)
	pf, _ := newTestPublisher(t, listerDB{newFakeDB()}, fresh.target(t), t.TempDir(), Options{})
	if _, err := pf.Round(ctx); err != nil {
		t.Fatal(err)
	}
	for k := range fresh.objects {
		if _, ok := s3.objects[k]; !ok {
			t.Errorf("%s missing after the resumed rounds", k)
		}
	}
	// When the catalog was first stored, every immutable object was already there.
	ci := slices.Index(all, "catalog.json")
	if ci < 0 {
		t.Fatal("catalog never stored")
	}
	before := map[string]bool{}
	for _, k := range all[:ci] {
		before[k] = true
	}
	for k := range fresh.objects {
		if immutableKey(k) && !before[k] {
			t.Errorf("%s stored after the first catalog", k)
		}
	}
}

func TestPublishAuthAndRejected(t *testing.T) {
	db := newFakeDB()
	db.final = true
	s3 := newFakeS3(t)
	ctx := context.Background()
	s3.fail = func(n int, key string) int {
		if key == "tile/m.s.player/0/0/0/2.bin" {
			return http.StatusBadRequest
		}
		return 0
	}
	p, _ := newTestPublisher(t, db, s3.target(t), t.TempDir(), Options{})
	st, err := p.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rejected != 1 {
		t.Fatalf("stats %v", st)
	}
	// The final indexes wait until every object they name is stored.
	for _, k := range []string{"timeline/m.s.player/0/2026-10-12.json", "movers/m.s/day/2026-10-12.json"} {
		if _, ok := s3.objects[k]; ok {
			t.Errorf("%s stored although a tile was refused", k)
		}
	}
	// Without a lister, every z0 tile's overlays are read and empty ones are not stored.
	if _, ok := s3.objects["overlays/m.s.player/1/0.json"]; ok || st.Empty != 1 {
		t.Fatalf("empty overlay stored (%v)", st)
	}
	if _, ok := s3.objects["overlays/m.s.player/0/0.json"]; !ok {
		t.Fatal("overlay with intervals not stored")
	}
	// The refused object is tried again in the next round.
	s3.fail = nil
	s3.takePuts()
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	second := s3.takePuts()
	for _, k := range []string{"tile/m.s.player/0/0/0/2.bin", "timeline/m.s.player/0/2026-10-12.json", "movers/m.s/day/2026-10-12.json"} {
		if !slices.Contains(second, k) {
			t.Errorf("%s not stored in the next round", k)
		}
	}

	// Wrong credentials stop the round at once.
	bad, _ := NewS3Target(S3Config{Endpoint: s3.srv.URL, Bucket: "map", Credentials: Credentials{AccessKeyID: "x", SecretAccessKey: "y", Region: "auto"}})
	p2, st2 := newTestPublisher(t, newFakeDB(), bad, t.TempDir(), Options{})
	_, err = p2.Round(ctx)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("bad credentials: %v", err)
	}
	if q, _ := st2.LoadQueue(); len(q) == 0 {
		t.Fatal("queue dropped after an auth failure")
	}
}

func TestPublishDefersFinalIndexesForMissingObjects(t *testing.T) {
	db := newFakeDB()
	db.final = true
	db.missing = map[string]bool{"tile/m.s.player/0/0/0/2.bin": true}
	s3 := newFakeS3(t)
	ctx := context.Background()
	p, _ := newTestPublisher(t, db, s3.target(t), t.TempDir(), Options{})
	st, err := p.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Missing != 1 {
		t.Fatalf("stats %v", st)
	}
	for _, k := range []string{"timeline/m.s.player/0/2026-10-12.json", "movers/m.s/day/2026-10-12.json"} {
		if _, ok := s3.objects[k]; ok {
			t.Errorf("%s stored although a tile it names was missing", k)
		}
	}
	// Once the object can be read, the next round stores it and the final indexes.
	db.missing = nil
	s3.takePuts()
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	second := s3.takePuts()
	for _, k := range []string{"tile/m.s.player/0/0/0/2.bin", "timeline/m.s.player/0/2026-10-12.json", "movers/m.s/day/2026-10-12.json"} {
		if !slices.Contains(second, k) {
			t.Errorf("%s not stored in the next round", k)
		}
	}
}

func TestKeyframeEvery(t *testing.T) {
	db := newFakeDB()
	db.latest = map[core.TileCoord]core.Version{{X: 0, Y: 0}: 10, {Z: -1}: 5}
	d, err := NewDirTarget(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newTestPublisher(t, db, d, t.TempDir(), Options{KeyframeEvery: 4, NoMovers: true, NoOverlays: true})
	if _, err := p.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	var full, coarse, deltas []string
	filepath.WalkDir(d.Root, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(d.Root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasPrefix(rel, "tile/m.s.player/0/"):
			full = append(full, filepath.Base(rel))
		case strings.HasPrefix(rel, "tile/m.s.player/-1/"):
			coarse = append(coarse, filepath.Base(rel))
		case strings.HasPrefix(rel, "tiledelta/"):
			deltas = append(deltas, filepath.Base(rel))
		}
		return nil
	})
	sort.Strings(full)
	if want := []string{"0.bin", "1.bin", "10.bin", "4.bin", "8.bin"}; !slices.Equal(full, want) {
		t.Errorf("z0 full versions %v, want %v", full, want)
	}
	if len(coarse) != 6 {
		t.Errorf("coarse versions %v, want all 6", coarse)
	}
	if len(deltas) != 9 {
		t.Errorf("deltas %v, want 9", deltas)
	}
}

func TestDirTargetAndStaticHandler(t *testing.T) {
	db := newFakeDB()
	out := filepath.Join(t.TempDir(), "out")
	d, err := NewDirTarget(out)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(out, ".tsdb-publish")
	p, _ := newTestPublisher(t, listerDB{db}, d, stateDir, Options{Web: testWeb})
	ctx := context.Background()
	if _, err := p.Round(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewStaticHandler(out))
	defer srv.Close()
	get := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := get("/catalog.json")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Cache-Control") != CacheCatalog {
		t.Fatalf("catalog: %d %v", res.StatusCode, res.Header)
	}
	if !strings.Contains(string(gunzipIf(t, fakeObject{body: body, header: res.Header})), `"static":true`) {
		t.Fatal("catalog body")
	}
	res = get("/assets/logo-abc.png")
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" || res.Header.Get("Content-Type") != "image/png" || res.Header.Get("Cache-Control") != CacheImmutable {
		t.Fatalf("png: %v", res.Header)
	}
	res = get("/")
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("index: %d %v", res.StatusCode, res.Header)
	}
	for _, path := range []string{"/.tsdb-publish/done.log", "/.tsdb-publish/target.json", "/../x", "/tile/nothing.bin", "/sub/.hidden/file.txt"} {
		res := get(path)
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}

	// A file deleted from the export is written again by the next round.
	victim := filepath.Join(out, "tile", fakeDS, "0", "0", "0", "2.bin")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	st, err := p.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil || st.Immutable != 1 {
		t.Fatalf("deleted file not restored (%v, %v)", err, st)
	}
}

func TestStateTornTailAndTarget(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenState(dir, "dir:/a", false)
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"a", "b", "a"} {
		if err := s.Record(k, record{Hash: "h" + string(rune('0'+i)), Size: int64(i), Immutable: k == "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "done.log"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("c\thalf")
	f.Close()
	s, err = OpenState(dir, "dir:/a", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := s.Lookup("a"); !ok || r.Hash != "h2" || r.Immutable {
		t.Fatalf("a: %+v %v", r, ok)
	}
	if r, ok := s.Lookup("b"); !ok || !r.Immutable {
		t.Fatalf("b: %+v %v", r, ok)
	}
	if _, ok := s.Lookup("c"); ok || s.Len() != 2 {
		t.Fatal("torn record kept")
	}
	s.Record("d", record{Hash: "x"})
	s.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "done.log"))
	if !strings.HasSuffix(string(b), "d\tx\t0\tm\n") || strings.Contains(string(b), "half") {
		t.Fatalf("log after torn tail: %q", b)
	}
	if _, err := OpenState(dir, "dir:/other", false); err == nil {
		t.Fatal("state opened for another target")
	}
	s, err = OpenState(dir, "dir:/other", true)
	if err != nil || s.Len() != 0 {
		t.Fatalf("reset: %v %d", err, s.Len())
	}
	s.Close()
}

func TestHandlerSourceAndDecode(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/catalog":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write([]byte(`{"version":1}`))
			zw.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			w.Write(buf.Bytes())
		default:
			w.WriteHeader(404)
		}
	})
	src := &HandlerSource{Handler: h, ReadToken: "tok"}
	r, err := src.Get(context.Background(), "catalog")
	if err != nil || string(r.Body) != `{"version":1}` || r.ContentType != "application/json" {
		t.Fatalf("%q %v", r.Body, err)
	}
	if _, err := src.Get(context.Background(), "tile/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hs := &HTTPSource{Base: srv.URL + "/api/v3", ReadToken: "tok"}
	r, err = hs.Get(context.Background(), "catalog")
	if err != nil || string(r.Body) != `{"version":1}` {
		t.Fatalf("http source: %q %v", r.Body, err)
	}
	if _, err := (&HTTPSource{Base: srv.URL + "/api/v3/"}).Get(context.Background(), "catalog"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("401: %v", err)
	}
}

func TestStaticIndex(t *testing.T) {
	got := string(staticIndex([]byte(`<head><meta name="tsdb-source" content="" /><link rel="stylesheet" href="/assets/a.css"></head>`)))
	if got != `<head><meta name="tsdb-source" content="static" /><link rel="stylesheet" href="./assets/a.css"></head>` {
		t.Fatal(got)
	}
	got = string(staticIndex([]byte(`<head></head>`)))
	if !strings.Contains(got, `<meta name="tsdb-source" content="static" />`) {
		t.Fatal(got)
	}
}
