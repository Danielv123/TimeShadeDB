package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"timeshadedb/internal/v3/core"
)

func init() {
	register(Command{
		Name:    "tail",
		Summary: "forward a standalone game's capture files (script-output/timeshade) to POST /api/v3/ingest",
		Group:   GroupV3,
		Run:     cmdTail,
	})
}

func cmdTail(ctx context.Context, args []string) error {
	return runTail(ctx, os.Stdout, os.Stderr, args)
}

// tailer forwards the capture files of the timeshade_capture mod (docs/v3/PROTOCOL.md 2):
// it posts files in name order, adding the forwarder fields to every envelope, deletes a
// file once ingest acknowledged all of it and a newer file exists, and posts the newest
// file up to its last complete line without deleting it.
type tailer struct {
	dir, url, token, src, mapKey string
	instance                     int64
	batchBytes                   int
	client                       *http.Client
	logf                         func(string, ...any)
	now                          func() time.Time
	sleep                        func(context.Context, time.Duration) error

	// offsets holds the bytes already acknowledged per file name.
	offsets map[string]int64
	seq     int64
	posted  int64 // envelopes acknowledged
}

// pass forwards what the directory holds now. It returns the number of envelopes
// posted. Transient ingest failures are retried with backoff until ctx ends.
func (t *tailer) pass(ctx context.Context) (int, error) {
	names, err := t.list()
	if err != nil {
		return 0, err
	}
	total := 0
	for i, name := range names {
		newest := i == len(names)-1
		n, err := t.forward(ctx, name, newest)
		total += n
		if err != nil {
			return total, err
		}
		if !newest {
			if err := os.Remove(filepath.Join(t.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return total, err
			}
			delete(t.offsets, name)
			t.logf("forwarded and deleted %s", name)
		}
	}
	// Forget offsets of files that disappeared.
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range t.offsets {
		if !keep[n] {
			delete(t.offsets, n)
		}
	}
	return total, nil
}

// list returns the capture files, sorted by name (the mod zero-pads <seq>-<tick>).
func (t *tailer) list() ([]string, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".ndjson") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// forward posts the complete lines of a file from its offset. A file that is not the
// newest is complete: a trailing partial line (the game stopped mid-write) is dropped.
func (t *tailer) forward(ctx context.Context, name string, newest bool) (int, error) {
	path := filepath.Join(t.dir, name)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	off := t.offsets[name]
	if fi.Size() < off {
		// The file was replaced (the same save loaded again): start over.
		t.logf("%s shrank from %d to %d bytes; reading it again", name, off, fi.Size())
		off = 0
	}
	if fi.Size() == off {
		return 0, nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, fi.Size()-off))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if end < len(data) && !newest {
		t.logf("%s: dropping %d bytes of an incomplete last line", name, len(data)-end)
	}
	data = data[:end]
	total := 0
	for len(data) > 0 {
		// Cut a batch at a line boundary.
		cut := len(data)
		if cut > t.batchBytes {
			cut = bytes.LastIndexByte(data[:t.batchBytes], '\n') + 1
			if cut == 0 {
				cut = bytes.IndexByte(data, '\n') + 1 // one line longer than a batch
			}
		}
		n, err := t.post(ctx, data[:cut])
		total += n
		if err != nil {
			return total, err
		}
		off += int64(cut)
		t.offsets[name] = off
		data = data[cut:]
	}
	return total, nil
}

// post sends one batch of envelope lines with the forwarder fields added, retrying
// until ingest answers 200 or ctx ends.
func (t *tailer) post(ctx context.Context, lines []byte) (int, error) {
	recv := core.FromTime(t.now())
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	n := 0
	for len(lines) > 0 {
		i := bytes.IndexByte(lines, '\n')
		line := bytes.TrimSpace(lines[:i])
		lines = lines[i+1:]
		if len(line) < 2 || line[0] != '{' {
			continue
		}
		t.seq++
		fmt.Fprintf(zw, `{"src":%s,"map":%s,"recv_ms":%d,"seq":%d`, jsonString(t.src), jsonString(t.mapKey), recv, t.seq)
		if t.instance != 0 {
			fmt.Fprintf(zw, `,"instance":%d`, t.instance)
		}
		rest := bytes.TrimSpace(line[1:])
		if len(rest) > 0 && rest[0] != '}' {
			zw.Write([]byte{','})
		}
		zw.Write(rest)
		zw.Write([]byte{'\n'})
		n++
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	backoff := 500 * time.Millisecond
	for {
		retry, err := t.send(ctx, body.Bytes())
		if err == nil {
			t.posted += int64(n)
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		wait := backoff
		if retry > 0 {
			wait = retry
		}
		t.logf("ingest: %v; retrying in %v", err, wait)
		if err := t.sleep(ctx, wait); err != nil {
			return 0, err
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// send posts a body once. It returns the server's Retry-After on 503.
func (t *tailer) send(ctx context.Context, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	res, err := t.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode == http.StatusOK {
		var ir core.IngestResponse
		if json.Unmarshal(b, &ir) == nil && (ir.BadLines > 0 || len(ir.Errors) > 0) {
			t.logf("ingest: %d bad lines, errors %q", ir.BadLines, ir.Errors)
		}
		return 0, nil
	}
	var retry time.Duration
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
		retry = time.Duration(s) * time.Second
	}
	return retry, fmt.Errorf("status %d: %s", res.StatusCode, bytes.TrimSpace(b))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

func runTail(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "the mod's capture directory (<factorio>/script-output/timeshade)")
	url := fs.String("url", "http://127.0.0.1:8080", "server base URL (or the full .../api/v3/ingest URL)")
	token := fs.String("token", "", "ingest bearer token")
	src := fs.String("src", "", "clock source id, unique per running game (default standalone:<map>)")
	mapKey := fs.String("map", "", "map key (required)")
	instance := fs.Int64("instance", 0, "instance id for overlay keys (0 standalone)")
	poll := fs.Duration("poll", time.Second, "how often to look for new lines")
	batch := fs.Int("batch-bytes", 1<<20, "largest request body before compression")
	once := fs.Bool("once", false, "forward what is there now and exit")
	quiet := fs.Bool("quiet", false, "log only errors")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fmt.Fprintln(stdout, "usage: timeshadedb tail --dir <script-output>/timeshade --map <key> [--url ...] [--token ...] [--src standalone:<name>]")
			fs.PrintDefaults()
			return nil
		}
		return fmt.Errorf("tail: %w", err)
	}
	if *dir == "" || *mapKey == "" {
		return errors.New("tail: --dir and --map are required")
	}
	if *src == "" {
		*src = "standalone:" + *mapKey
	}
	if *batch < 1024 {
		return errors.New("tail: --batch-bytes must be at least 1024")
	}
	target := strings.TrimRight(*url, "/")
	if !strings.HasSuffix(target, "/ingest") {
		target += "/api/v3/ingest"
	}
	logger := log.New(stderr, "tail: ", log.LstdFlags)
	logf := logger.Printf
	if *quiet {
		logf = func(f string, a ...any) {
			if strings.Contains(f, "ingest:") {
				logger.Printf(f, a...)
			}
		}
	}
	t := &tailer{
		dir: *dir, url: target, token: *token, src: *src, mapKey: *mapKey, instance: *instance,
		batchBytes: *batch, client: &http.Client{Timeout: 60 * time.Second}, logf: logf,
		now: time.Now, sleep: sleepCtx, offsets: map[string]int64{},
		seq: time.Now().UnixMilli() * 1000, // increasing across restarts
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		n, err := t.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tail: %w", err)
		}
		if n > 0 && !*quiet {
			logf("posted %d envelopes (%d total)", n, t.posted)
		}
		if *once {
			fmt.Fprintf(stdout, "posted %d envelopes\n", t.posted)
			return nil
		}
		if err := sleepCtx(ctx, *poll); err != nil {
			return nil
		}
	}
}
