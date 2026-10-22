package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
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

	"timeshadedb/internal/core"
)

func init() {
	register(Command{
		Name:    "tail",
		Summary: "forward a standalone game's capture files (script-output/timeshade) to POST /api/v3/ingest",
		Run:     cmdTail,
	})
}

func cmdTail(ctx context.Context, args []string) error {
	return runTail(ctx, os.Stdout, os.Stderr, args)
}

// tailer forwards the capture files of the timeshadedb_exporter mod (docs/PROTOCOL.md 2):
// it posts files in name order, adding the forwarder fields to every envelope, deletes a
// file once ingest acknowledged all of it and a newer file exists, and posts the newest
// file up to its last complete line without deleting it. Every line gets the time the game
// wrote it as recv_ms (lineStamps), so a backlog keeps its times however late it is
// posted. Once everything is posted it sends caught_up envelopes (heartbeat). The
// acknowledged offsets are kept in a state file, so a restarted tailer does not post lines
// again.
type tailer struct {
	dir, url, token, src, mapKey string
	instance                     int64
	batchBytes                   int
	client                       *http.Client
	logf                         func(string, ...any)
	now                          func() time.Time
	sleep                        func(context.Context, time.Duration) error
	// statePath is the state file ("" keeps offsets in memory only).
	statePath string

	// offsets holds what was acknowledged per file name.
	offsets map[string]*tailFile
	seq     int64
	posted  int64 // envelopes acknowledged
	// lastRecv is the newest recv_ms stamped on a line (kept in the state file): stamps
	// increase line by line. lastTick is the newest tick posted, for caught_up envelopes.
	lastRecv int64
	lastTick int64
	// lastSent is when the tailer last posted (data or caught_up), for heartbeat.
	lastSent time.Time
}

// tailFile is the state of one capture file: the bytes acknowledged, and a hash of its
// first bytes, so a file replaced under the same name is not resumed mid-way.
type tailFile struct {
	Off     int64  `json:"off"`
	HeadLen int    `json:"head_len"`
	Head    string `json:"head"`
	checked bool   // the head was compared with the file since the state was loaded
}

// tailHeadBytes is how much of a file's start the state hashes.
const tailHeadBytes = 1024

type tailState struct {
	Version  int                  `json:"version"`
	Files    map[string]*tailFile `json:"files"`
	LastRecv int64                `json:"last_recv,omitempty"`
}

// loadState reads the state file, if any.
func (t *tailer) loadState() error {
	if t.statePath == "" {
		return nil
	}
	b, err := os.ReadFile(t.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st tailState
	if err := json.Unmarshal(b, &st); err != nil || st.Version != 1 {
		t.logf("ignoring unreadable state file %s: %v", t.statePath, err)
		return nil
	}
	for name, f := range st.Files {
		if f != nil && f.Off > 0 && filepath.Base(name) == name {
			t.offsets[name] = f
		}
	}
	t.lastRecv = max(t.lastRecv, st.LastRecv)
	return nil
}

// saveState writes the state file atomically.
func (t *tailer) saveState() error {
	if t.statePath == "" {
		return nil
	}
	b, err := json.Marshal(tailState{Version: 1, Files: t.offsets, LastRecv: t.lastRecv})
	if err != nil {
		return err
	}
	tmp := t.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.statePath)
}

// fileHead hashes the first n bytes of f.
func fileHead(f *os.File, n int) (string, error) {
	b := make([]byte, n)
	got, err := f.ReadAt(b, 0)
	if got < n {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	h := fnv.New64a()
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 16), nil
}

// pass forwards what the directory holds now. It returns the number of envelopes
// posted. Transient ingest failures are retried with backoff until ctx ends.
func (t *tailer) pass(ctx context.Context) (int, error) {
	names, err := t.list()
	if err != nil {
		return 0, err
	}
	ends := t.fileEnds(names)
	total := 0
	for i, name := range names {
		newest := i == len(names)-1
		n, err := t.forward(ctx, name, newest, ends[i])
		total += n
		if err != nil {
			return total, err
		}
		if !newest {
			if err := os.Remove(filepath.Join(t.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return total, err
			}
			delete(t.offsets, name)
			if err := t.saveState(); err != nil {
				return total, err
			}
			t.logf("forwarded and deleted %s", name)
		}
	}
	// Forget offsets of files that disappeared.
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	forgot := false
	for n := range t.offsets {
		if !keep[n] {
			delete(t.offsets, n)
			forgot = true
		}
	}
	if forgot {
		return total, t.saveState()
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
func (t *tailer) forward(ctx context.Context, name string, newest bool, wrote time.Time) (int, error) {
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
	st := t.offsets[name]
	if st == nil {
		st = &tailFile{checked: true}
	}
	off := st.Off
	switch {
	case fi.Size() < off:
		// The file was replaced (the same save loaded again): start over.
		t.logf("%s shrank from %d to %d bytes; reading it again", name, off, fi.Size())
		*st = tailFile{checked: true}
		off = 0
	case !st.checked && st.HeadLen > 0:
		// Resuming from the state file: make sure it is still the same file.
		if h, err := fileHead(f, st.HeadLen); err != nil || h != st.Head {
			t.logf("%s changed since the state was saved; reading it again", name)
			*st = tailFile{}
			off = 0
		}
	}
	st.checked = true
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
	if wrote.IsZero() {
		wrote = fi.ModTime()
	}
	lines := t.lineStamps(data, wrote)
	total := 0
	for len(lines) > 0 {
		// Cut a batch at a line boundary.
		cut, size := 0, 0
		for cut < len(lines) && (cut == 0 || size+len(lines[cut].b) <= t.batchBytes) {
			size += len(lines[cut].b)
			cut++
		}
		n, err := t.postLines(ctx, lines[:cut])
		total += n
		if err != nil {
			return total, err
		}
		lines = lines[cut:]
		off += int64(size)
		st.Off = off
		if st.HeadLen < tailHeadBytes && off > int64(st.HeadLen) {
			st.HeadLen = int(min(off, tailHeadBytes))
			if st.Head, err = fileHead(f, st.HeadLen); err != nil {
				return total, err
			}
		}
		t.offsets[name] = st
		if err := t.saveState(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// fileEnds returns, per file (in name order), when the game wrote its last line: its
// modification time, but no later than the clock and, going back from the newest file,
// no later than the next file's last line minus the ticks between their last lines (at
// the next file's speed), or than the next file's first line when the ticks do not
// continue. So files whose modification times are later than their ticks allow (copied
// without them, all at once) are laid out back from the newest by their ticks and do not
// overlap, while a pause between files (an earlier modification time) is kept. A file
// whose ticks cannot be read keeps its modification time (zero: forward stats it).
func (t *tailer) fileEnds(names []string) []time.Time {
	type span struct {
		first, last int64
		speed       float64
		mtime       time.Time
		ok          bool
	}
	spans := make([]span, len(names))
	for i, name := range names {
		f, err := os.Open(filepath.Join(t.dir, name))
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil {
			head := make([]byte, min(fi.Size(), 512))
			tailBytes := make([]byte, min(fi.Size(), 16<<10))
			_, err1 := f.ReadAt(head, 0)
			_, err2 := f.ReadAt(tailBytes, fi.Size()-int64(len(tailBytes)))
			if (err1 == nil || errors.Is(err1, io.EOF)) && (err2 == nil || errors.Is(err2, io.EOF)) {
				first, _ := lineTick(head)
				// the last complete line
				end := bytes.LastIndexByte(tailBytes, '\n')
				if end > 0 {
					start := bytes.LastIndexByte(tailBytes[:end], '\n') + 1
					last, speed := lineTick(tailBytes[start:end])
					spans[i] = span{first, last, speed, fi.ModTime(), first >= 0 && last >= first}
				}
			}
			if !spans[i].ok {
				spans[i].mtime = fi.ModTime()
			}
		}
		f.Close()
	}
	ends := make([]time.Time, len(names))
	now := t.now()
	for i := len(names) - 1; i >= 0; i-- {
		s := spans[i]
		if s.mtime.IsZero() {
			continue
		}
		e := s.mtime
		if e.After(now) {
			e = now
		}
		if i+1 < len(names) && s.ok && spans[i+1].ok && !ends[i+1].IsZero() {
			n := spans[i+1]
			var bound time.Time
			if n.first >= s.last {
				bound = ends[i+1].Add(-time.Duration(float64(n.last-s.last) * float64(time.Second) / (60 * n.speed)))
			} else {
				bound = ends[i+1].Add(-time.Duration(float64(n.last-n.first) * float64(time.Second) / (60 * n.speed)))
			}
			if bound.Before(e) {
				e = bound
			}
		}
		ends[i] = e
	}
	return ends
}

// gameIdle is how long the newest capture file must be unchanged before the tailer takes
// the game for stopped (it appends every 2 s while it runs, PROTOCOL.md 2).
const gameIdle = 10 * time.Second

// gameStopped reports whether the game looks stopped: no capture file, or the newest one
// unchanged for gameIdle.
func (t *tailer) gameStopped() bool {
	names, err := t.list()
	if err != nil {
		return false
	}
	if len(names) == 0 {
		return true
	}
	fi, err := os.Stat(filepath.Join(t.dir, names[len(names)-1]))
	return err == nil && t.now().Sub(fi.ModTime()) >= gameIdle
}

// finish sends the last caught_up envelope when the tailer stops: ended when the game
// looks stopped, so the server does not wait for more of this source.
func (t *tailer) finish() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ended := t.gameStopped()
	if err := t.caughtUp(ctx, ended); err != nil {
		return err
	}
	if ended {
		t.logf("the game looks stopped: told the server this source ended")
	}
	return t.saveState()
}

// tline is one line to post (with its newline) and the recv_ms it is stamped with.
type tline struct {
	b    []byte
	recv int64
}

// lineStamps splits data (complete lines, the rest of a file from its acknowledged
// offset) and stamps each line with when the game wrote it (docs/PROTOCOL.md 1): the
// last line with the file's end time (fileEnds: its modification time, no later than now
// or than its successor allows), each line before it that time minus the ticks to the next
// line at 60 x speed ticks a second (a tick that
// goes back, a reloaded save, counts as no time). When they would start at or before the
// newest stamp of an earlier line, all of them move up by the difference, as far as the
// clock allows; within the lines they increase strictly. A game that ran slower than its
// speed, or paused, gives stamps later than the truth, which the server's clock anchor
// allows (stamps earlier than the truth would make it place the data too early). Files
// whose modification times do not match their ticks (copied without them, say) give
// stamps that go back at file boundaries; the anchor then starts over there.
func (t *tailer) lineStamps(data []byte, end time.Time) []tline {
	var lines []tline
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n') + 1
		lines = append(lines, tline{b: data[:i]})
		data = data[i:]
	}
	if len(lines) == 0 {
		return nil
	}
	now := core.FromTime(t.now())
	last := min(core.FromTime(end), now)
	ticks := make([]int64, len(lines))
	speeds := make([]float64, len(lines))
	for i, l := range lines {
		ticks[i], speeds[i] = lineTick(l.b)
	}
	ms := float64(last)
	lines[len(lines)-1].recv = int64(last)
	for i := len(lines) - 2; i >= 0; i-- {
		if d := ticks[i+1] - ticks[i]; ticks[i] >= 0 && ticks[i+1] >= 0 && d > 0 {
			ms -= float64(d) * 1000 / (60 * speeds[i+1])
		}
		lines[i].recv = int64(ms)
	}
	if shift := min(t.lastRecv+1-lines[0].recv, int64(now)-lines[len(lines)-1].recv); shift > 0 {
		for i := range lines {
			lines[i].recv += shift
		}
	}
	for i := 1; i < len(lines); i++ {
		lines[i].recv = max(lines[i].recv, lines[i-1].recv+1)
	}
	for _, tk := range ticks {
		t.lastTick = max(t.lastTick, tk)
	}
	return lines
}

// lineTick reads an envelope line's tick and speed (1 when absent) from its first
// fields, as the capture writes them ({"v":1,"tick":T[,"speed":S],"msgs":...); -1 when
// there is no tick.
func lineTick(line []byte) (int64, float64) {
	head := line[:min(len(line), 96)]
	tick, speed := int64(-1), 1.0
	if i := bytes.Index(head, []byte(`"tick":`)); i >= 0 {
		rest := head[i+7:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if v, err := strconv.ParseInt(string(rest[:j]), 10, 64); err == nil {
			tick = v
		}
	}
	if i := bytes.Index(head, []byte(`"speed":`)); i >= 0 {
		rest := head[i+8:]
		j := 0
		for j < len(rest) && (rest[j] >= '0' && rest[j] <= '9' || rest[j] == '.' || rest[j] == 'e' || rest[j] == 'E' || rest[j] == '-' || rest[j] == '+') {
			j++
		}
		if v, err := strconv.ParseFloat(string(rest[:j]), 64); err == nil && v > 0 && v <= 1000 {
			speed = v
		}
	}
	return tick, speed
}

// caughtUp posts a caught_up envelope: everything the tailer read was posted
// (docs/PROTOCOL.md 1), so the server need not hold the map for this source beyond
// now. With ended it says the game stopped, and the server stops holding the map for the
// source at all until it sends again.
func (t *tailer) caughtUp(ctx context.Context, ended bool) error {
	recv := max(int64(core.FromTime(t.now())), t.lastRecv+1)
	t.seq++
	var line bytes.Buffer
	fmt.Fprintf(&line, `{"src":%s,"map":%s,"recv_ms":%d,"seq":%d`, jsonString(t.src), jsonString(t.mapKey), recv, t.seq)
	if t.instance != 0 {
		fmt.Fprintf(&line, `,"instance":%d`, t.instance)
	}
	if ended {
		line.WriteString(`,"ended":true`)
	}
	fmt.Fprintf(&line, `,"v":1,"tick":%d,"caught_up":true,"msgs":[]}`+"\n", max(t.lastTick, 0))
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	zw.Write(line.Bytes())
	if err := zw.Close(); err != nil {
		return err
	}
	if err := t.sendRetry(ctx, body.Bytes()); err != nil {
		return err
	}
	t.lastSent = t.now()
	return nil
}

// statusError is a status other than 200 from ingest.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d: %s", e.code, e.msg) }

// postLines posts lines; when ingest refuses the body itself (400, or 413 for a body
// over its cap), it splits the batch, and skips, loudly, a single line it refuses.
func (t *tailer) postLines(ctx context.Context, lines []tline) (int, error) {
	n, err := t.post(ctx, lines)
	var se *statusError
	if err == nil || !errors.As(err, &se) || (se.code != http.StatusBadRequest && se.code != http.StatusRequestEntityTooLarge) {
		return n, err
	}
	if len(lines) == 1 {
		t.logf("ingest: skipping a line ingest refuses (%v): %.200s", se, lines[0].b)
		return 0, nil
	}
	mid := len(lines) / 2
	n1, err := t.postLines(ctx, lines[:mid])
	if err != nil {
		return n1, err
	}
	n2, err := t.postLines(ctx, lines[mid:])
	return n1 + n2, err
}

// post sends one batch of envelope lines with the forwarder fields added. It retries
// network errors, 5xx, 429 and 408 (the body arrived too slowly) with backoff, honouring
// Retry-After, until ingest answers 200 or ctx ends; any other status is returned as a
// *statusError. A retried body is the same bytes (the same recv_ms and seq), which the
// server skips where it applied them already.
func (t *tailer) post(ctx context.Context, lines []tline) (int, error) {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	n := 0
	for _, tl := range lines {
		line := bytes.TrimSpace(tl.b)
		if len(line) < 2 || line[0] != '{' {
			continue
		}
		t.seq++
		t.lastRecv = max(t.lastRecv, tl.recv)
		fmt.Fprintf(zw, `{"src":%s,"map":%s,"recv_ms":%d,"seq":%d`, jsonString(t.src), jsonString(t.mapKey), tl.recv, t.seq)
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
	if err := t.sendRetry(ctx, body.Bytes()); err != nil {
		return 0, err
	}
	t.posted += int64(n)
	t.lastSent = t.now()
	return n, nil
}

// sendRetry posts a body until ingest answers 200 or ctx ends, retrying network errors,
// 5xx, 429 and 408 with backoff (honouring Retry-After); any other status is returned as
// a *statusError.
func (t *tailer) sendRetry(ctx context.Context, body []byte) error {
	backoff := 500 * time.Millisecond
	for {
		retry, err := t.send(ctx, body)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var se *statusError
		if errors.As(err, &se) && se.code != http.StatusTooManyRequests && se.code != http.StatusRequestTimeout && se.code < 500 {
			return se
		}
		wait := backoff
		if retry > 0 {
			wait = retry
		}
		t.logf("ingest: %v; retrying in %v", err, wait)
		if err := t.sleep(ctx, wait); err != nil {
			return err
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
	return retry, &statusError{res.StatusCode, string(bytes.TrimSpace(b))}
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
	heartbeat := fs.Duration("heartbeat", 5*time.Second, "while there is nothing new to post, send a caught_up envelope this often, so the server does not hold the map for this source (0: never; one is always sent when tail stops, marked ended when the game looks stopped)")
	state := fs.String("state", "", "file that keeps the acknowledged offsets across restarts (default <dir>/timeshadedb-tail.state.json; - for none)")
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
	switch *state {
	case "":
		*state = filepath.Join(*dir, "timeshadedb-tail.state.json")
	case "-":
		*state = ""
	}
	t := &tailer{
		dir: *dir, url: target, token: *token, src: *src, mapKey: *mapKey, instance: *instance,
		batchBytes: *batch, client: &http.Client{Timeout: 60 * time.Second}, logf: logf,
		now: time.Now, sleep: sleepCtx, offsets: map[string]*tailFile{}, statePath: *state,
		seq: time.Now().UnixMilli() * 1000, // increasing across restarts
	}
	if err := t.loadState(); err != nil {
		return fmt.Errorf("tail: state: %w", err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopped := func() error {
		if err := t.finish(); err != nil {
			logf("ingest: final caught_up not sent: %v", err)
		}
		return nil
	}
	for {
		n, err := t.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return stopped()
			}
			return fmt.Errorf("tail: %w", err)
		}
		if n > 0 && !*quiet {
			logf("posted %d envelopes (%d total)", n, t.posted)
		}
		if *once {
			if err := t.finish(); err != nil {
				return fmt.Errorf("tail: %w", err)
			}
			fmt.Fprintf(stdout, "posted %d envelopes\n", t.posted)
			return nil
		}
		if n == 0 && *heartbeat > 0 && t.now().Sub(t.lastSent) >= *heartbeat {
			if err := t.caughtUp(ctx, false); err != nil {
				if ctx.Err() != nil {
					return stopped()
				}
				return fmt.Errorf("tail: %w", err)
			}
			if err := t.saveState(); err != nil {
				return fmt.Errorf("tail: state: %w", err)
			}
		}
		if err := sleepCtx(ctx, *poll); err != nil {
			return stopped()
		}
	}
}
