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

	"timeshadedb/internal/core"
	"timeshadedb/internal/import/minimap/minimaptest"
)

func TestServeFlags(t *testing.T) {
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
		_, err := parseServe(io.Discard, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if o, err := parseServe(io.Discard, []string{"--db", "x", "--listen", "0.0.0.0:0", "--ingest-token", "t"}); err != nil || o.ingestToken != "t" {
		t.Fatalf("token on a public address: %v", err)
	}
	if o, err := parseServe(io.Discard, []string{"--db", "x", "--listen", "[::1]:0"}); err != nil || o == nil {
		t.Fatalf("loopback v6: %v", err)
	}
}

// startServe runs serve on a free port and returns its base URL and a stop function
// that waits for a clean shutdown.
func startServe(t *testing.T, args ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, pw, io.Discard, append([]string{"--listen", "127.0.0.1:0", "--no-viewer", "--quiet"}, args...))
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
					t.Errorf("serve: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("serve did not stop")
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

// TestServeImportedDB imports the synthetic illisis export, serves it and reads the
// catalog, a view, its tiles, a coarse tile and the movers, then stops the server.
func TestServeImportedDB(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	db := filepath.Join(t.TempDir(), "db")
	importIntoDB(t, exp.Root, db)
	base, stop := startServe(t, "--db", db)
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
	base, _ = startServe(t, "--db", db)
	getJSON(t, base+"/api/v3/catalog", &cat)
}

// TestTailIntoServe forwards the capture fixture (raw Lua output) with `tail` into a
// fresh serve and checks what was stored.
func TestTailIntoServe(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "capture", "test", "fixtures", "game_envelopes.ndjson"))
	if err != nil {
		t.Skip("capture fixture missing:", err)
	}
	db := filepath.Join(t.TempDir(), "db")
	base, _ := startServe(t, "--db", db, "--ingest-token", "secret")
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
	// The state file kept the offsets: a second run posts nothing again.
	out.Reset()
	if err := runTail(context.Background(), &out, io.Discard, []string{"--dir", dir, "--url", base, "--token", "secret", "--map", "lab", "--once", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "posted 0 envelopes") {
		t.Fatalf("restarted tail output %q", out.String())
	}
	// Without the token, ingest is refused: tail stops with an error instead of
	// retrying forever, and keeps the file.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	os.WriteFile(filepath.Join(dir, "0000000003-0000001200.ndjson"), []byte(lines[0]+"\n"), 0o644)
	if err := runTail(ctx, io.Discard, io.Discard, []string{"--dir", dir, "--url", base, "--map", "lab", "--quiet"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("tail without a token: %v", err)
	}
	// 0002 was acknowledged in full and is no longer the newest file; 0003 stays.
	if _, err := os.Stat(filepath.Join(dir, "0000000003-0000001200.ndjson")); err != nil {
		t.Fatal("file deleted although ingest refused it")
	}
}

// fakeIngest records posted envelopes; fail makes the next n posts answer 503, codes
// answers the next posts with these statuses, maxEnvs answers 413 to bodies with more
// envelopes, and a body with an envelope of tick 666 gets 400.
type fakeIngest struct {
	mu      sync.Mutex
	envs    []map[string]any
	fail    int
	codes   []int
	maxEnvs int
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
	if len(f.codes) > 0 {
		code := f.codes[0]
		f.codes = f.codes[1:]
		w.WriteHeader(code)
		return
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var envs []map[string]any
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m["tick"] == 666.0 {
			w.WriteHeader(400)
			return
		}
		envs = append(envs, m)
	}
	if f.maxEnvs > 0 && len(envs) > f.maxEnvs {
		w.WriteHeader(413)
		return
	}
	f.envs = append(f.envs, envs...)
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
		sleep:     func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		offsets:   map[string]*tailFile{},
		statePath: filepath.Join(t.TempDir(), "state.json")}
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
	// recv_ms: the first read's last line at the clock (the file is newer than the fake
	// clock), the line before it a tick earlier; never after the (stopped) clock.
	var lastSeq float64
	for i, e := range fi.envs {
		if e["src"] != "standalone:x" || e["map"] != "m" || e["instance"].(float64) != 3 || e["v"].(float64) != 1 {
			t.Fatalf("envelope %d: %v", i, e)
		}
		if s := e["seq"].(float64); s <= lastSeq {
			t.Fatalf("seq not increasing at %d", i)
		} else {
			lastSeq = s
		}
		if r := e["recv_ms"].(float64); r > 5000 {
			t.Fatalf("recv_ms at %d: %v after the clock", i, r)
		}
	}
	if fi.envs[0]["recv_ms"].(float64) != 4983 || fi.envs[1]["recv_ms"].(float64) != 5000 {
		t.Fatalf("first stamps %v %v", fi.envs[0]["recv_ms"], fi.envs[1]["recv_ms"])
	}
	if fi.envs[2]["tick"].(float64) != 3 {
		t.Fatalf("completed line: %v", fi.envs[2])
	}
	// A shrunk file (the same save loaded again) is read from the start.
	os.WriteFile(filepath.Join(dir, "b.ndjson"), []byte(env(7)), 0o644)
	if n, _ := tl.pass(ctx); n != 1 {
		t.Fatalf("shrunk file: %d", n)
	}

	// A restarted tailer resumes from the state file: nothing is posted again.
	write("b.ndjson", env(8))
	restart := func() *tailer {
		t2 := *tl
		t2.offsets = map[string]*tailFile{}
		if err := t2.loadState(); err != nil {
			t.Fatal(err)
		}
		return &t2
	}
	tl = restart()
	if n, err := tl.pass(ctx); err != nil || n != 1 {
		t.Fatalf("pass after restart: %d %v", n, err)
	}
	tl = restart()
	if n, _ := tl.pass(ctx); n != 0 {
		t.Fatalf("restart re-posted %d envelopes", n)
	}
	// A file replaced under the same name (longer, different start) is read again.
	os.WriteFile(filepath.Join(dir, "b.ndjson"), []byte(env(900)+env(901)+env(902)), 0o644)
	tl = restart()
	if n, _ := tl.pass(ctx); n != 3 {
		t.Fatalf("replaced file: %d", n)
	}

	// Statuses: a batch over the cap is split, a line ingest refuses is skipped, and
	// a wrong token stops the tailer.
	before := len(fi.envs)
	fi.maxEnvs = 2
	write("b.ndjson", env(10)+env(11)+env(666)+env(12)+env(13))
	if n, err := tl.pass(ctx); err != nil || n != 4 {
		t.Fatalf("split and skip: %d %v", n, err)
	}
	if got := len(fi.envs) - before; got != 4 {
		t.Fatalf("%d envelopes after split and skip", got)
	}
	fi.maxEnvs = 0
	// 408 (the body arrived too slowly) is retried like 503.
	fi.codes = []int{408}
	before = len(fi.envs)
	write("b.ndjson", env(15))
	if n, err := tl.pass(ctx); err != nil || n != 1 || len(fi.envs)-before != 1 {
		t.Fatalf("408 retry: %d %v", n, err)
	}
	fi.codes = []int{401}
	write("b.ndjson", env(14))
	if _, err := tl.pass(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("401: %v", err)
	}
}

// TestTailStampsFromFile: lines a game wrote earlier get their own times, from the file's
// modification time back by ticks (a backlog keeps its times), and caught_up follows.
func TestTailStampsFromFile(t *testing.T) {
	fi := &fakeIngest{}
	ts := httptest.NewServer(fi)
	defer ts.Close()
	dir := t.TempDir()
	written := time.UnixMilli(1_791_000_000_000)
	now := written.Add(time.Hour) // posted an hour later
	tl := &tailer{dir: dir, url: ts.URL, src: "standalone:x", mapKey: "m", batchBytes: 1 << 20,
		client: ts.Client(), logf: func(string, ...any) {}, now: func() time.Time { return now },
		sleep: func(context.Context, time.Duration) error { return nil }, offsets: map[string]*tailFile{}}
	path := filepath.Join(dir, "a.ndjson")
	lines := `{"v":1,"tick":0,"msgs":[]}` + "\n" + `{"v":1,"tick":600,"msgs":[]}` + "\n" + `{"v":1,"tick":600,"msgs":[]}` + "\n" +
		`{"v":1,"tick":1200,"speed":2,"msgs":[]}` + "\n"
	os.WriteFile(path, []byte(lines), 0o644)
	os.Chtimes(path, written, written)
	if n, err := tl.pass(context.Background()); err != nil || n != 4 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if err := tl.finish(); err != nil {
		t.Fatal(err)
	}
	w := written.UnixMilli()
	want := []int64{w - 5000 - 10000, w - 5000, w - 5000 + 1, w}
	for i, e := range fi.envs[:4] {
		if got := int64(e["recv_ms"].(float64)); got != want[i] {
			t.Fatalf("line %d stamped %d, want %d", i, got, want[i])
		}
	}
	cu := fi.envs[4]
	if cu["caught_up"] != true || cu["ended"] != true || int64(cu["recv_ms"].(float64)) != now.UnixMilli() || cu["tick"].(float64) != 1200 || len(cu["msgs"].([]any)) != 0 {
		t.Fatalf("caught_up envelope %v (the file is an hour old: the game stopped)", cu)
	}
	// A game still writing: the final caught_up does not say it ended.
	now = written.Add(5 * time.Second)
	if err := tl.finish(); err != nil {
		t.Fatal(err)
	}
	if cu := fi.envs[len(fi.envs)-1]; cu["caught_up"] != true || cu["ended"] != nil {
		t.Fatalf("caught_up of a running game %v", cu)
	}
}

// TestTailStampsCopiedFiles: files that all got the same modification time (copied
// without it) are laid out back from the newest one by their ticks, so a backlog of them
// does not overlap in time; a pause between files (an earlier modification time) stays.
func TestTailStampsCopiedFiles(t *testing.T) {
	fi := &fakeIngest{}
	ts := httptest.NewServer(fi)
	defer ts.Close()
	dir := t.TempDir()
	copied := time.UnixMilli(1_791_000_000_000)
	tl := &tailer{dir: dir, url: ts.URL, src: "standalone:x", mapKey: "m", batchBytes: 1 << 20,
		client: ts.Client(), logf: func(string, ...any) {}, now: func() time.Time { return copied.Add(time.Minute) },
		sleep: func(context.Context, time.Duration) error { return nil }, offsets: map[string]*tailFile{}}
	env := func(tick int) string { return `{"v":1,"tick":` + itoa(int32(tick)) + `,"msgs":[]}` + "\n" }
	write := func(name string, mtime time.Time, ticks ...int) {
		var b strings.Builder
		for _, tk := range ticks {
			b.WriteString(env(tk))
		}
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(b.String()), 0o644)
		os.Chtimes(p, mtime, mtime)
	}
	// 1 and 2 copied at once (one minute of ticks each); 3 written after a pause, later.
	write("1.ndjson", copied, 0, 1800, 3600)
	write("2.ndjson", copied, 3720, 5400, 7200)
	write("3.ndjson", copied.Add(30*time.Second), 9000)
	tl.now = func() time.Time { return copied.Add(30 * time.Second) }
	if n, err := tl.pass(context.Background()); err != nil || n != 7 {
		t.Fatalf("pass: %d %v", n, err)
	}
	c := copied.UnixMilli()
	// 3 ends at its own time; 2 at its own time (where 3.s ticks put it too);
	// 1 is laid back from 2 by the 3,600 ticks between their last lines.
	want := []int64{c - 60000 - 60000, c - 60000 - 30000, c - 60000, c - 58000, c - 30000, c, c + 30000}
	for i, e := range fi.envs {
		if got := int64(e["recv_ms"].(float64)); got != want[i] {
			t.Fatalf("line %d stamped %d, want %d", i, got-c, want[i]-c)
		}
	}
}

func itoa(v int32) string   { return strconv.FormatInt(int64(v), 10) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func TestIconsCopiedAndServed(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "icons"), 0o755)
	os.WriteFile(filepath.Join(root, "icons", "sheet.abc.png"), []byte("\x89PNG fake"), 0o644)
	os.WriteFile(filepath.Join(root, "icons", "meta.abc.json"), []byte(`{"item":[]}`), 0o644)
	db := filepath.Join(t.TempDir(), "db")
	if err := copyIcons(db, root, &core.CatalogIcons{PNG: "icons/sheet.abc.png", JSON: "icons/meta.abc.json"}); err != nil {
		t.Fatal(err)
	}
	if err := copyIcons(db, root, &core.CatalogIcons{PNG: "icons/../x y.png", JSON: "icons/meta.abc.json"}); err == nil {
		t.Fatal("bad icon name accepted")
	}
	base, _ := startServe(t, "--db", db)
	var cat core.Catalog
	getJSON(t, base+"/api/v3/catalog", &cat)
	if cat.Icons == nil || cat.Icons.PNG != "icons/sheet.abc.png" || cat.Icons.JSON != "icons/meta.abc.json" {
		t.Fatalf("icons %+v", cat.Icons)
	}
	res, err := http.Get(base + "/api/v3/" + cat.Icons.PNG)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != "\x89PNG fake" || res.Header.Get("Content-Type") != "image/png" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("icon: %d %q %v", res.StatusCode, b, res.Header)
	}
	for _, p := range []string{"icons/icons.json", "icons/..%2Fmaps.json", "icons/other.png"} {
		if s := getStatus(t, base+"/api/v3/"+p); s != 404 {
			t.Fatalf("%s: %d", p, s)
		}
	}
}

// TestServeShutdownWithLiveClient: an open live stream does not delay shutdown.
func TestServeShutdownWithLiveClient(t *testing.T) {
	base, stop := startServe(t, "--db", filepath.Join(t.TempDir(), "db"))
	res, err := http.Get(base + "/api/v3/live")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("live: %d", res.StatusCode)
	}
	t0 := time.Now()
	stop()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("shutdown took %v with a live client", d)
	}
}

// TestServeDataClockMoversFirst: on the data clock a fresh database's first body may
// be mover samples recorded long ago, spanning more than the commit grace; none of them
// is late or "ahead" of the clock.
func TestServeDataClockMoversFirst(t *testing.T) {
	db := filepath.Join(t.TempDir(), "db")
	base, stop := startServe(t, "--db", db, "--clock", "data", "--commit-every", "20ms")
	defer stop()
	start := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	var body strings.Builder
	const n = 600 // 10 minutes of data at one sample per second
	for i := range n {
		env := map[string]any{"v": 1, "tick": i * 60, "src": "s", "map": "m", "recv_ms": start + int64(i)*1000, "msgs": []any{
			map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "1", "k": "t", "f": "player", "x": i, "y": 0, "h": 0.25}}},
		}}
		b, _ := json.Marshal(env)
		body.Write(b)
		body.WriteByte('\n')
	}
	resp, err := http.Post(base+"/api/v3/ingest", "application/x-ndjson", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ir core.IngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d err %v", resp.StatusCode, err)
	}
	if ir.Samples.Accepted != n {
		t.Fatalf("samples %+v (errors %v)", ir.Samples, ir.Errors)
	}
}
