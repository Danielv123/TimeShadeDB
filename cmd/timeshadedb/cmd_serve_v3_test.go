package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"timeshadedb/internal/import/minimap/minimaptest"
	"timeshadedb/internal/v3/core"
)

func TestServeV3Flags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--db is required"},
		{[]string{"--db", "x", "--listen", "0.0.0.0:0"}, "--ingest-token is required"},
		{[]string{"--db", "x", "--listen", "nope"}, "bad --listen"},
		{[]string{"--db", "x", "--mem-budget", "1000"}, "--mem-budget"},
		{[]string{"--db", "x", "extra"}, "unexpected arguments"},
	} {
		_, err := parseServeV3(io.Discard, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if o, err := parseServeV3(io.Discard, []string{"--db", "x", "--listen", "0.0.0.0:0", "--ingest-token", "t"}); err != nil || o.ingestToken != "t" {
		t.Fatalf("token on a public address: %v", err)
	}
	if o, err := parseServeV3(io.Discard, []string{"--db", "x", "--listen", "[::1]:0"}); err != nil || o == nil {
		t.Fatalf("loopback v6: %v", err)
	}
}

// startServeV3 runs serve-v3 on a free port and returns its base URL and a stop function
// that waits for a clean shutdown.
func startServeV3(t *testing.T, args ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runServeV3(ctx, pw, io.Discard, append([]string{"--listen", "127.0.0.1:0", "--no-viewer", "--quiet"}, args...))
		pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	if !sc.Scan() {
		cancel()
		t.Fatalf("no output: %v", <-done)
	}
	addr, ok := strings.CutPrefix(sc.Text(), "listening on ")
	if !ok {
		cancel()
		t.Fatalf("first line %q", sc.Text())
	}
	go io.Copy(io.Discard, pr)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve-v3: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("serve-v3 did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return "http://" + addr, stop
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// TestServeV3ImportedDB imports the synthetic illisis export, serves it and reads the
// catalog, a view, its tiles, a coarse tile and the movers, then stops the server.
func TestServeV3ImportedDB(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	db := filepath.Join(t.TempDir(), "db")
	importIntoDB(t, exp.Root, db)
	base, stop := startServeV3(t, "--db", db)
	if s := getStatus(t, base+"/healthz"); s != 200 {
		t.Fatalf("healthz %d", s)
	}
	var cat core.Catalog
	getJSON(t, base+"/api/v3/catalog", &cat)
	if len(cat.Maps) == 0 {
		t.Fatal("empty catalog")
	}
	checked := 0
	for _, m := range cat.Maps {
		for _, s := range m.Surfaces {
			for _, d := range s.Datastores {
				b := d.Bounds
				var vr core.ViewResponse
				getJSON(t, base+"/api/v3/view/"+d.ID+"/0?bbox="+itoa(b[0])+","+itoa(b[1])+","+itoa(b[2])+","+itoa(b[3]), &vr)
				if len(vr.Tiles) == 0 {
					t.Fatalf("%s: empty view", d.ID)
				}
				for _, vt := range vr.Tiles {
					if s := getStatus(t, base+"/api/v3/tile/"+d.ID+"/0/"+itoa(vt.X)+"/"+itoa(vt.Y)+"/"+itoa(int32(vt.Version))+".bin"); s != 200 {
						t.Fatalf("%s tile %d,%d v%d: %d", d.ID, vt.X, vt.Y, vt.Version, s)
					}
					checked++
				}
				if len(d.Levels) > 1 {
					z := d.Levels[len(d.Levels)-1]
					getJSON(t, base+"/api/v3/view/"+d.ID+"/"+itoa(int32(z))+"?bbox=-2,-2,2,2", &vr)
					if len(vr.Tiles) == 0 {
						t.Fatalf("%s: empty coarse view at %d", d.ID, z)
					}
				}
			}
			if s.Movers != nil && len(s.Movers.Days) > 0 {
				var day core.DayIndex
				getJSON(t, base+"/api/v3/movers/"+s.ID+"/day/"+s.Movers.Days[0]+".json", &day)
				if len(day.Windows) == 0 {
					t.Fatalf("%s: no windows", s.ID)
				}
				if st := getStatus(t, base+"/api/v3/movers/"+s.ID+"/window/"+itoa64(int64(day.Windows[0].Start))+".bin"); st != 200 {
					t.Fatalf("window: %d", st)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no tiles checked")
	}
	stop()
	// The stores were closed cleanly: a second server opens the same database.
	base, _ = startServeV3(t, "--db", db)
	getJSON(t, base+"/api/v3/catalog", &cat)
}

// TestTailIntoServeV3 forwards the capture fixture (raw Lua output) with `tail` into a
// fresh serve-v3 and checks what was stored.
func TestTailIntoServeV3(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "capture", "test", "fixtures", "game_envelopes.ndjson"))
	if err != nil {
		t.Skip("capture fixture missing:", err)
	}
	db := filepath.Join(t.TempDir(), "db")
	base, _ := startServeV3(t, "--db", db, "--ingest-token", "secret")
	dir := t.TempDir()
	lines := strings.SplitAfter(strings.TrimSpace(string(fixture)), "\n")
	half := len(lines) / 2
	os.WriteFile(filepath.Join(dir, "0000000001-0000000000.ndjson"), []byte(strings.Join(lines[:half], "")+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "0000000002-0000000600.ndjson"), []byte(strings.Join(lines[half:], "")+"\n"), 0o644)
	var out bytes.Buffer
	if err := runTail(context.Background(), &out, io.Discard, []string{"--dir", dir, "--url", base, "--token", "secret", "--map", "lab", "--once", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "posted "+itoa(int32(len(lines)))+" envelopes") {
		t.Fatalf("tail output %q", out.String())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if len(left) != 1 || filepath.Base(left[0]) != "0000000002-0000000600.ndjson" {
		t.Fatalf("files left %v", left)
	}
	// The ingested chunks are live content in the new datastore.
	var cat core.Catalog
	getJSON(t, base+"/api/v3/catalog", &cat)
	if len(cat.Maps) != 1 || cat.Maps[0].Key != "lab" || len(cat.Maps[0].Surfaces) == 0 {
		t.Fatalf("catalog %+v", cat)
	}
	found := false
	for _, s := range cat.Maps[0].Surfaces {
		for _, d := range s.Datastores {
			if d.Last > 0 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no datastore with data: %+v", cat.Maps[0].Surfaces)
	}
	// Without the token, ingest is refused and tail keeps retrying; a cancelled tail
	// stops cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	os.WriteFile(filepath.Join(dir, "0000000003-0000001200.ndjson"), []byte(lines[0]+"\n"), 0o644)
	if err := runTail(ctx, io.Discard, io.Discard, []string{"--dir", dir, "--url", base, "--map", "lab", "--quiet"}); err != nil {
		t.Fatalf("cancelled tail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0000000002-0000000600.ndjson")); err != nil {
		t.Fatal("file deleted although ingest refused it")
	}
}

// fakeIngest records posted envelopes; fail makes the next n posts answer 503.
type fakeIngest struct {
	mu   sync.Mutex
	envs []map[string]any
	fail int
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(503)
		return
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			w.WriteHeader(400)
			return
		}
		f.envs = append(f.envs, m)
	}
	io.WriteString(w, `{"envelopes":1,"bad_lines":0,"errors":[]}`)
}

func TestTailerFiles(t *testing.T) {
	fi := &fakeIngest{}
	ts := httptest.NewServer(fi)
	defer ts.Close()
	dir := t.TempDir()
	var slept []time.Duration
	tl := &tailer{dir: dir, url: ts.URL, src: "standalone:x", mapKey: "m", instance: 3, batchBytes: 1024,
		client: ts.Client(), logf: func(string, ...any) {}, now: func() time.Time { return time.UnixMilli(5000) },
		sleep:   func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		offsets: map[string]int64{}}
	ctx := context.Background()
	write := func(name, s string) {
		f, _ := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	env := func(tick int) string { return `{"v":1,"tick":` + itoa(int32(tick)) + `,"msgs":[]}` + "\n" }
	// The newest file is posted up to its last complete line and kept.
	write("a.ndjson", env(1)+env(2)+`{"v":1,"ti`)
	if n, err := tl.pass(ctx); err != nil || n != 2 {
		t.Fatalf("pass 1: %d %v", n, err)
	}
	if n, _ := tl.pass(ctx); n != 0 {
		t.Fatal("re-posted acknowledged lines")
	}
	// The line completes; a newer file appears: a.ndjson is finished and deleted.
	write("a.ndjson", `ck":3,"msgs":[]}`+"\n")
	write("b.ndjson", env(4))
	fi.fail = 1
	if n, err := tl.pass(ctx); err != nil || n != 2 {
		t.Fatalf("pass 2: %d %v", n, err)
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Fatalf("503 backoff %v", slept)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.ndjson")); !os.IsNotExist(err) {
		t.Fatal("finished file not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.ndjson")); err != nil {
		t.Fatal("newest file deleted")
	}
	// Large files go in several batches, cut at lines.
	var big strings.Builder
	for i := range 100 {
		big.WriteString(env(100 + i))
	}
	write("b.ndjson", big.String())
	if n, err := tl.pass(ctx); err != nil || n != 100 {
		t.Fatalf("pass 3: %d %v", n, err)
	}
	if len(fi.envs) != 104 {
		t.Fatalf("%d envelopes", len(fi.envs))
	}
	var lastSeq float64
	for i, e := range fi.envs {
		if e["src"] != "standalone:x" || e["map"] != "m" || e["recv_ms"].(float64) != 5000 || e["instance"].(float64) != 3 || e["v"].(float64) != 1 {
			t.Fatalf("envelope %d: %v", i, e)
		}
		if s := e["seq"].(float64); s <= lastSeq {
			t.Fatalf("seq not increasing at %d", i)
		} else {
			lastSeq = s
		}
	}
	if fi.envs[2]["tick"].(float64) != 3 {
		t.Fatalf("completed line: %v", fi.envs[2])
	}
	// A shrunk file (the same save loaded again) is read from the start.
	os.WriteFile(filepath.Join(dir, "b.ndjson"), []byte(env(7)), 0o644)
	if n, _ := tl.pass(ctx); n != 1 {
		t.Fatalf("shrunk file: %d", n)
	}
}

func itoa(v int32) string   { return strconv.FormatInt(int64(v), 10) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
