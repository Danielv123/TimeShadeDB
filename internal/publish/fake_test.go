package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"timeshadedb/internal/core"
)

// fakeDB is a Source over a tiny synthetic database: one map, one surface, one
// datastore with levels 0 and -1, one mover day, overlays on one tile.
type fakeDB struct {
	mu     sync.Mutex
	day0   core.TimeMs
	latest map[core.TileCoord]core.Version // per tile (z, x, y)
	final  bool                            // timelines final
	liveN  int
	gets   map[string]int
	empty  bool // overlays answer no intervals
	lister bool // implements OverlayLister through ListOverlayTiles
	// missing answers not found for these paths (an object gone between planning and reading).
	missing map[string]bool
}

const fakeDS = "m.s.player"
const fakeSfc = "m.s"

func newFakeDB() *fakeDB {
	day0, _ := core.ParseUTCDay("2026-10-12")
	return &fakeDB{
		day0: day0,
		latest: map[core.TileCoord]core.Version{
			{Z: 0, X: 0, Y: 0}:  3,
			{Z: 0, X: 1, Y: 0}:  2,
			{Z: -1, X: 0, Y: 0}: 2,
		},
		gets: map[string]int{},
	}
}

func (f *fakeDB) now() time.Time { return time.UnixMilli(int64(f.day0) + 12*3600*1000) }

// bump adds a version to a tile.
func (f *fakeDB) bump(t core.TileCoord) {
	f.mu.Lock()
	f.latest[t]++
	f.mu.Unlock()
}

func pad(s string) []byte { return []byte(strings.Repeat(s+"\n", 40)) }

func (f *fakeDB) Get(ctx context.Context, path string) (Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets[path]++
	bin := func(b []byte) (Resource, error) {
		return Resource{Body: b, ContentType: "application/octet-stream"}, nil
	}
	js := func(v any) (Resource, error) {
		b, err := json.Marshal(v)
		return Resource{Body: b, ContentType: "application/json"}, err
	}
	nf := func() (Resource, error) { return Resource{}, fmt.Errorf("get %s: %w", path, ErrNotFound) }
	if f.missing[path] {
		return nf()
	}
	day := core.UTCDay(f.day0)
	switch {
	case path == "catalog":
		return js(core.Catalog{
			Version: 1, Now: f.day0 + 10_000, LiveMs: 5000,
			Icons: &core.CatalogIcons{PNG: "icons/sheet.png", JSON: "icons/sheet.json"},
			Maps: []core.CatalogMap{{MapInfo: core.MapInfo{Key: "m", Title: "M"}, Surfaces: []core.CatalogSurface{{
				Name: "s", ID: fakeSfc,
				Datastores: []core.CatalogDatastore{{Force: "player", ID: fakeDS, WindowMs: 60_000, First: f.day0 + 1000, Last: f.day0 + 5000, PaletteVersion: 3, Levels: []int8{0, -1}}},
				Movers:     &core.CatalogMovers{First: f.day0, Last: f.day0 + 1000, Days: []string{day}},
			}}}},
		})
	case strings.HasPrefix(path, "timeline/"+fakeDS+"/"):
		parts := strings.Split(path, "/")
		z, _ := strconv.Atoi(parts[2])
		if parts[3] != day+".json" {
			return nf()
		}
		tl := core.TimelineResponse{DS: fakeDS, Z: int8(z), Date: day, Start: f.day0, Final: f.final, Tiles: []core.TimelineTile{}}
		for tc, v := range f.latest {
			if int(tc.Z) != z {
				continue
			}
			tt := core.TimelineTile{X: tc.X, Y: tc.Y, Points: [][2]int64{}}
			for i := core.Version(1); i <= v; i++ {
				tt.Points = append(tt.Points, [2]int64{int64(f.day0) + int64(i)*60_000, int64(i)})
			}
			tl.Tiles = append(tl.Tiles, tt)
		}
		sort.Slice(tl.Tiles, func(i, j int) bool {
			a, b := tl.Tiles[i], tl.Tiles[j]
			return a.Y < b.Y || a.Y == b.Y && a.X < b.X
		})
		return js(tl)
	case strings.HasPrefix(path, "tile/"+fakeDS+"/"):
		var z, x, y int
		var v uint
		if _, err := fmt.Sscanf(path, "tile/"+fakeDS+"/%d/%d/%d/%d.bin", &z, &x, &y, &v); err != nil {
			return nf()
		}
		if l, ok := f.latest[core.TileCoord{Z: int8(z), X: int32(x), Y: int32(y)}]; !ok || core.Version(v) > l {
			return nf()
		}
		return bin(pad(path))
	case strings.HasPrefix(path, "tiledelta/"+fakeDS+"/"):
		var x, y int
		var a, b uint
		if _, err := fmt.Sscanf(path, "tiledelta/"+fakeDS+"/%d/%d/%d-%d.bin", &x, &y, &a, &b); err != nil {
			return nf()
		}
		if l, ok := f.latest[core.TileCoord{X: int32(x), Y: int32(y)}]; !ok || core.Version(b) > l || b != a+1 {
			return nf()
		}
		return bin(pad(path))
	case path == "palette/"+fakeDS+"/3.bin":
		return bin([]byte("TSP3palette"))
	case path == "icons/sheet.png":
		return Resource{Body: pad("png"), ContentType: "image/png"}, nil
	case path == "icons/sheet.json":
		return js(map[string]string{"icons": "x"})
	case path == "movers/"+fakeSfc+"/day/"+day+".json":
		return js(core.DayIndex{Date: day, Final: true,
			Windows:   []core.WindowRef{{Start: f.day0, Rows: 3}, {Start: f.day0 + 300_000, Rows: 1}},
			Keyframes: []core.TimeMs{f.day0 + 3600_000}})
	case strings.HasPrefix(path, "movers/"+fakeSfc+"/window/"), strings.HasPrefix(path, "movers/"+fakeSfc+"/keyframe/"):
		return bin(pad(path))
	case path == "movers/"+fakeSfc+"/live.bin":
		f.liveN++
		return bin(pad(fmt.Sprintf("live %d", f.liveN)))
	case strings.HasPrefix(path, "overlays/"+fakeDS+"/"):
		r := core.OverlayResponse{DS: fakeDS, Version: 1, Intervals: []core.Interval{}}
		if !f.empty && path == "overlays/"+fakeDS+"/0/0.json" {
			r.Intervals = append(r.Intervals, core.Interval{Kind: "tag", Key: "k", Start: f.day0})
		}
		return js(r)
	}
	return nf()
}

// listerDB adds the OverlayLister.
type listerDB struct{ *fakeDB }

func (l listerDB) OverlayTiles(ctx context.Context, ds string) ([]core.TileCoord, error) {
	return []core.TileCoord{{X: 0, Y: 0}}, nil
}

var testCreds = Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret/test", Region: "auto"}

// fakeS3 is an S3 endpoint that checks the SigV4 signature of every PUT (as S3 does:
// over the headers the request lists as signed, with the body's hash) and keeps the
// objects. fail, if set, picks a status for a request (0: accept).
type fakeS3 struct {
	t      *testing.T
	srv    *httptest.Server
	bucket string

	mu      sync.Mutex
	objects map[string]fakeObject
	puts    []string // keys in order of accepted PUTs
	fail    func(n int, key string) int
	n       int
}

type fakeObject struct {
	body   []byte
	header http.Header
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{t: t, bucket: "map", objects: map[string]fakeObject{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPut {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	i := strings.Index(auth, "SignedHeaders=")
	if i < 0 {
		http.Error(w, "no auth", http.StatusForbidden)
		return
	}
	names := strings.Split(strings.SplitN(auth[i+len("SignedHeaders="):], ",", 2)[0], ";")
	headers := map[string]string{}
	for _, n := range names {
		if n == "host" {
			headers[n] = r.Host
		} else {
			headers[n] = r.Header.Get(n)
		}
	}
	if headers["x-amz-content-sha256"] != sha256Hex(body) || auth != authorization(testCreds, "PUT", r.URL.EscapedPath(), headers) {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+f.bucket+"/")
	if !ok {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.fail != nil {
		if s := f.fail(f.n, key); s != 0 {
			w.WriteHeader(s)
			return
		}
	}
	f.objects[key] = fakeObject{body: body, header: r.Header.Clone()}
	f.puts = append(f.puts, key)
}

func (f *fakeS3) target(t *testing.T) *S3Target {
	s, err := NewS3Target(S3Config{Endpoint: f.srv.URL, Bucket: f.bucket, Credentials: testCreds})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// takePuts returns and clears the accepted PUTs.
func (f *fakeS3) takePuts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.puts
	f.puts = nil
	return p
}
