// Package ingest decodes capture envelopes (docs/v3/PROTOCOL.md) and applies them to the
// v3 stores: chunk charts to the terrain store, move messages through the journey rules
// to the mover store, tags, recipes and rails to the overlay store, and "map" messages
// to <db>/maps.json. It owns the per-source clock anchors (PROTOCOL.md 4) that turn game
// ticks into map-clock milliseconds. The HTTP API (POST /api/v3/ingest) and tests call
// Ingest with an NDJSON body; `timeshadedb tail` posts files to that route.
package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/movers"
)

// Stores are the stores ingest writes to. Any of them may be nil, in which case its
// messages are counted as invalid.
type Stores struct {
	Terrain  core.TerrainStore
	Movers   core.MoverStore
	Overlays core.OverlayStore
	Maps     *Maps
}

// Notifier is told what ingest applied, for live events (API.md 5). Calls happen with
// the ingest lock held, in application order; implementations must not block.
type Notifier interface {
	// ChunksApplied is called with every chunk update of a batch the terrain store took
	// (updates it counted as late or invalid included; the store does not say which, so
	// the receiver should compare tile states rather than trust the updates).
	ChunksApplied(updates []core.ChunkUpdate)
	// SamplesApplied is called with the mover samples of a batch the mover store took,
	// again including the ones it counted as late or invalid: the receiver filters them
	// by the store's rules (core.MoverStore) before showing them.
	SamplesApplied(samples []core.MoverSample)
	// MapsChanged is called when maps.json changed.
	MapsChanged()
}

// Options tune an Ingester. Zero values select the defaults.
type Options struct {
	// MaxLine bounds one NDJSON line (default 4 MiB, API.md 5); longer lines are bad lines.
	MaxLine int
	// MaxMessage bounds one message's JSON text (default 256 KiB); longer messages are
	// bad messages. A move message holds at most MaxMoveEntries entries.
	MaxMessage int
	// FlushChunks, FlushSamples and FlushEvents bound the decoded values held before
	// they are applied to the stores (defaults 2,048 chunk updates, 12,288 samples and
	// 16,384 overlay events). With the message bounds and the decode window they bound
	// ingest memory per request whatever the body. (A batch's samples are held twice
	// while the applier runs them through the journey rules.)
	FlushChunks, FlushSamples, FlushEvents int
	// MaxAhead: a message time more than this after the server clock is clamped to the
	// clock (default 10 s; < 0 disables). A broken anchor or game clock would otherwise
	// hold terrain open windows and mover open rows far in the future.
	MaxAhead core.TimeMs
	// Clock returns the server's map clock (default: wall clock).
	Clock func() core.TimeMs
	// DataClock, if set, advances to every valid envelope's recv_ms before its messages
	// are timed (pass DataClock.Now as Clock too, for a server replaying recorded
	// captures; messages are then timed against DataClock.Seen, and DataClock.Now is
	// published only once the rows read before it were applied). One envelope moves it at most MaxDataJump forward (default 1 h) and
	// never past the wall clock plus MaxAhead, so a single bad recv_ms cannot push the
	// map clock far ahead; such envelopes count as clamped.
	DataClock   *DataClock
	MaxDataJump core.TimeMs
	// Sources, if set, keeps per source the newest applied time (the horizon holds of the
	// commit loop) and the last applied envelope, whose retries it skips (sources.go).
	Sources *Sources
	// MaxSources bounds the clock anchors (default 4,096).
	MaxSources int
	// MaxJourneys bounds the tracked journeys per map (default 100,000), MaxMaps the maps
	// with a journey tracker (default 256; the least recently used is forgotten).
	MaxJourneys, MaxMaps int
	// Workers decode charts in parallel (default min(GOMAXPROCS, 8)).
	Workers int
	// Notifier receives what was applied (optional).
	Notifier Notifier
	// Logf receives trace messages and decode problems (default: discard).
	Logf func(format string, args ...any)
}

// Stats are cumulative counters for /metrics.
type Stats struct {
	Envelopes, Bytes, BadLines  int64
	Chunks, Samples, Overlays   core.ApplyResult
	BadMessages, Clamped, Maps  int64
	Overloaded                  int64
	OwnerDrops, UnknownGones    int64
	SurfaceChanges, Quarantined int64
	// ResolvedGones are gones of journeys the journey rules had forgotten (a server
	// restart), completed from the mover store's state.
	ResolvedGones int64
	// Duplicates are envelopes skipped because their source's retry repeated them
	// (Sources.Duplicate); CaughtUp are caught_up envelopes; OneRecv are envelopes that
	// carried the receive time of their source's previous envelope although their tick
	// was more than two seconds later: a forwarder that stamps a backlog with one time,
	// which the clock anchor cannot spread back out (their samples collide and the
	// mover store keeps one per journey and time, movers.Stats.Duplicates).
	Duplicates, CaughtUp, OneRecv int64
}

// Ingester applies capture envelopes to the stores. It is safe for concurrent use;
// bodies are applied one at a time, so the messages of one source keep their order.
type Ingester struct {
	st  Stores
	opt Options

	mu      sync.Mutex
	anchors *movers.Anchors
	// jmu guards the journeys, which the applier runs (with the mover store's Append, so
	// a refused batch rolls their changes back).
	jmu      sync.Mutex
	journeys map[string]*mapJourneys
	jtick    uint64
	// seg and win are the reusable read state of the body being applied (under mu).
	seg segment
	win window

	stats struct {
		envelopes, bytes, badLines, badMessages, clamped, maps, overloaded atomic.Int64
		duplicates, caughtUp, oneRecv                                      atomic.Int64
		chunks, samples, overlays                                          applyCounters
	}
}

type mapJourneys struct {
	js   *movers.Journeys
	used uint64
}

type applyCounters struct{ accepted, late, invalid atomic.Int64 }

func (c *applyCounters) add(r core.ApplyResult) {
	c.accepted.Add(int64(r.Accepted))
	c.late.Add(int64(r.Late))
	c.invalid.Add(int64(r.Invalid))
}

func (c *applyCounters) get() core.ApplyResult {
	return core.ApplyResult{Accepted: int(c.accepted.Load()), Late: int(c.late.Load()), Invalid: int(c.invalid.Load())}
}

// New returns an ingester over the stores.
func New(st Stores, opt Options) *Ingester {
	if opt.MaxLine <= 0 {
		opt.MaxLine = 4 << 20
	}
	if opt.MaxMessage <= 0 {
		opt.MaxMessage = 256 << 10
	}
	if opt.MaxDataJump <= 0 {
		opt.MaxDataJump = core.Hour
	}
	if opt.FlushChunks <= 0 {
		opt.FlushChunks = 2048
	}
	if opt.FlushSamples <= 0 {
		opt.FlushSamples = 12288
	}
	if opt.FlushEvents <= 0 {
		opt.FlushEvents = 16384
	}
	if opt.MaxAhead == 0 {
		opt.MaxAhead = 10 * core.Second
	}
	if opt.Clock == nil {
		opt.Clock = WallClock
	}
	if opt.MaxJourneys <= 0 {
		opt.MaxJourneys = 100_000
	}
	if opt.MaxMaps <= 0 {
		opt.MaxMaps = 256
	}
	if opt.Workers <= 0 {
		opt.Workers = min(runtime.GOMAXPROCS(0), 8)
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Ingester{st: st, opt: opt, anchors: movers.NewAnchors(opt.MaxSources), journeys: map[string]*mapJourneys{}}
}

// Stats returns the cumulative counters.
func (in *Ingester) Stats() Stats {
	s := Stats{
		Envelopes:   in.stats.envelopes.Load(),
		Bytes:       in.stats.bytes.Load(),
		BadLines:    in.stats.badLines.Load(),
		BadMessages: in.stats.badMessages.Load(),
		Clamped:     in.stats.clamped.Load(),
		Maps:        in.stats.maps.Load(),
		Overloaded:  in.stats.overloaded.Load(),
		Duplicates:  in.stats.duplicates.Load(),
		CaughtUp:    in.stats.caughtUp.Load(),
		OneRecv:     in.stats.oneRecv.Load(),
		Chunks:      in.stats.chunks.get(),
		Samples:     in.stats.samples.get(),
		Overlays:    in.stats.overlays.get(),
	}
	in.jmu.Lock()
	for _, mj := range in.journeys {
		js := mj.js.Stats()
		s.OwnerDrops += js.OwnerDrops
		s.UnknownGones += js.UnknownGones
		s.SurfaceChanges += js.SurfaceChanges
		s.Quarantined += js.Quarantined
		s.ResolvedGones += js.ResolvedGones
	}
	in.jmu.Unlock()
	return s
}

// AnchorOffsets calls fn with every source's clock anchor offset (metrics).
func (in *Ingester) AnchorOffsets(fn func(src string, offset float64)) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.anchors.Offsets(fn)
}

// Ingest reads an NDJSON body of capture envelopes and applies it. Bad lines and bad
// messages are counted in the response, not returned as errors. It returns an error
// when a store failed (a write error) or the read failed; the batches before it were
// applied, and retrying the whole body is safe: envelopes of a source up to the last one
// applied are skipped (Options.Sources), the journey rules of the failed batch were rolled
// back, a replayed envelope does not reset its clock anchor, terrain updates are absolute,
// samples dedupe on (journey, t) and unchanged overlay events are no-ops.
func (in *Ingester) Ingest(ctx context.Context, body io.Reader) (core.IngestResponse, error) {
	return in.ingest(ctx, body, nil)
}

// IngestBytes is Ingest on a body already in memory (the HTTP API reads bodies before
// applying them): lines are parsed in place instead of being copied out of a reader.
// data must not change until it returns.
func (in *Ingester) IngestBytes(ctx context.Context, data []byte) (core.IngestResponse, error) {
	return in.ingest(ctx, nil, data)
}

func (in *Ingester) ingest(ctx context.Context, body io.Reader, data []byte) (core.IngestResponse, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	b := &batch{in: in, ap: in.startApplier(ctx)}
	b.resp.Errors = []string{}
	in.seg.reset()
	var err error
	if body != nil {
		err = b.read(ctx, body)
	} else {
		err = b.readBytes(ctx, data)
	}
	in.seg.reset()
	b.finishLine()
	if err == nil {
		err = b.flush()
	}
	// The applier finishes the batches already handed over; its error wins, because
	// the reader only stops early when the applier failed.
	aerr := b.ap.finish()
	if aerr == nil && err == nil && in.opt.DataClock != nil {
		in.opt.DataClock.Publish(in.opt.DataClock.Seen())
	}
	b.resp.Chunks.Add(b.ap.res.chunks)
	b.resp.Samples.Add(b.ap.res.samples)
	b.resp.Overlays.Add(b.ap.res.overlays)
	if aerr != nil {
		return b.resp, aerr
	}
	return b.resp, err
}

// read reads, parses and queues the body's lines, handing full batches to the applier.
func (b *batch) read(ctx context.Context, body io.Reader) error {
	in := b.in
	br := bufio.NewReaderSize(body, 64<<10)
	seg := &in.seg
	lineNo := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, tooLong, err := readLine(br, seg.next(), in.opt.MaxLine)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		eof := errors.Is(err, io.EOF)
		if len(line) > 0 || tooLong {
			lineNo++
			in.stats.bytes.Add(int64(len(line)))
			seg.add(lineNo, line, tooLong)
		} else {
			seg.recycle(line)
		}
		if seg.full() || (eof && len(seg.lines) > 0) {
			if err := b.applySegment(seg); err != nil {
				return err
			}
			seg.reset()
		}
		if eof {
			return nil
		}
	}
}

// readBytes is read on an in-memory body: the segment's lines are slices of data.
func (b *batch) readBytes(ctx context.Context, data []byte) error {
	in := b.in
	seg := &in.seg
	lineNo := 0
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		tooLong := len(line) > in.opt.MaxLine
		if tooLong {
			line = nil
		}
		if len(line) > 0 || tooLong {
			lineNo++
			in.stats.bytes.Add(int64(len(line)))
			seg.add(lineNo, line, tooLong)
			seg.lines[len(seg.lines)-1].borrowed = true
		}
		if seg.full() || (len(data) == 0 && len(seg.lines) > 0) {
			if err := b.applySegment(seg); err != nil {
				return err
			}
			seg.reset()
		}
	}
	return nil
}

// applySegment parses the segment's envelopes in parallel, then decodes and applies
// their messages in order, a window at a time: decoded messages, and everything queued
// for the stores, stay bounded however many messages a line packs.
func (b *batch) applySegment(seg *segment) error {
	in := b.in
	parallel(in.opt.Workers, len(seg.lines), 1, func(i int) { seg.lines[i].parseEnvelope() })
	w := &in.win
	for li := 0; li < len(seg.lines); {
		w.items = w.items[:0]
		size := 0
		for li < len(seg.lines) && len(w.items) < windowMsgs && size < windowBytes {
			l := &seg.lines[li]
			if !l.begun {
				l.begun = true
				w.items = append(w.items, winItem{line: li, msg: -1})
			}
			raw, ok := l.it.next()
			if !ok {
				li++
				continue
			}
			w.items = append(w.items, winItem{line: li, msg: l.nmsg, raw: raw})
			l.nmsg++
			size += len(raw)
		}
		if cap(w.msgs) < len(w.items) {
			w.msgs = make([]wireMsg, len(w.items), max(len(w.items), windowMsgs+1))
		}
		if len(w.pix) < len(w.items) {
			w.pix = make([]core.ChunkPixels, max(len(w.items), windowMsgs+1))
		}
		msgs := w.msgs[:len(w.items)]
		maxMsg := in.opt.MaxMessage
		parallel(in.opt.Workers, len(w.items), 8, func(k int) {
			it := &w.items[k]
			if it.msg < 0 {
				return
			}
			m := &msgs[k]
			*m = decodeMsg(it.raw, maxMsg)
			// Inflate the chart here too, in parallel; chunk() checks the rest.
			if d, ok := m.chart(); ok && m.err == nil && m.Type == core.MsgChunk && len(d) > 0 && len(d) <= maxChart {
				if m.pxErr = decodeChartBytes(d, &w.pix[k]); m.pxErr == nil {
					m.px = &w.pix[k]
				}
			}
		})
		for k := range w.items {
			it := &w.items[k]
			if it.msg < 0 {
				b.beginLine(&seg.lines[it.line])
			} else if b.cur.ok {
				b.applyMsg(it.msg, &msgs[k])
			}
			msgs[k] = wireMsg{}
			it.raw = nil
			if b.full() {
				if it.msg >= 0 && (k+1 == len(w.items) || w.items[k+1].line != it.line) && seg.lines[it.line].it.empty() {
					// The line's last message: the batch holds all of it.
					b.finishLine()
				}
				if err := b.flush(); err != nil {
					return err
				}
			}
		}
	}
	b.finishLine()
	b.cur = lineState{}
	return nil
}

// Window bounds: messages are decoded in parallel in groups of up to this many messages
// or bytes of message text. A window's decoded charts take up to windowMsgs x 2 KiB.
const (
	windowMsgs  = 2048
	windowBytes = 1 << 20
)

// maxChart bounds a chunk message's chart text.
const maxChart = 64 << 10

// window is the reusable state of applySegment. pix holds the decoded charts of the
// window's chunk messages (index = item), copied into the batch when applied.
type window struct {
	items []winItem
	msgs  []wireMsg
	pix   []core.ChunkPixels
}

// winItem is a line start (msg < 0) or one message of a line.
type winItem struct {
	line, msg int
	raw       []byte
}

// segment holds raw lines of a body that are parsed together.
type segment struct {
	lines   []parsedLine
	bytes   int
	pool    [][]byte // line buffers for reuse
	poolCap int
}

// Segment bounds: envelopes parse in parallel in groups of up to this many lines or
// bytes; line buffers up to poolBuf bytes are kept for reuse, up to poolBytes in all.
const (
	segmentLines = 1024
	segmentBytes = 4 << 20
	poolBuf      = 64 << 10
	poolBytes    = 8 << 20
)

// parsedLine is one body line and, after parseEnvelope, its envelope header and an
// iterator over its messages (spans of raw).
type parsedLine struct {
	no       int
	raw      []byte
	borrowed bool // raw is a slice of the caller's body, not a pooled buffer
	tooLong  bool
	blank    bool
	env      envHeader
	it       arrayIter
	err      error // the line is not a valid envelope
	begun    bool  // applySegment queued the line start
	nmsg     int   // messages queued so far
}

// envHeader is core.Envelope without its messages, which are scanned separately.
type envHeader struct {
	V        int         `json:"v"`
	Tick     int64       `json:"tick"`
	Speed    float64     `json:"speed,omitempty"`
	Src      string      `json:"src,omitempty"`
	Map      string      `json:"map,omitempty"`
	Instance int64       `json:"instance,omitempty"`
	RecvMs   core.TimeMs `json:"recv_ms,omitempty"`
	Seq      *int64      `json:"seq,omitempty"`
	CaughtUp bool        `json:"caught_up,omitempty"`
	Ended    bool        `json:"ended,omitempty"`
}

func (s *segment) next() []byte {
	if n := len(s.pool); n > 0 {
		b := s.pool[n-1][:0]
		s.pool[n-1] = nil
		s.pool = s.pool[:n-1]
		s.poolCap -= cap(b)
		return b
	}
	return make([]byte, 0, 4096)
}

// recycle keeps a line buffer for reuse if it is small and the pool has room.
func (s *segment) recycle(b []byte) {
	if c := cap(b); c > 0 && c <= poolBuf && s.poolCap+c <= poolBytes {
		s.pool = append(s.pool, b)
		s.poolCap += c
	}
}

func (s *segment) add(no int, line []byte, tooLong bool) {
	s.lines = append(s.lines, parsedLine{no: no, raw: line, tooLong: tooLong})
	s.bytes += len(line)
}

func (s *segment) full() bool { return len(s.lines) >= segmentLines || s.bytes >= segmentBytes }

func (s *segment) reset() {
	for i := range s.lines {
		if !s.lines[i].borrowed {
			s.recycle(s.lines[i].raw)
		}
		s.lines[i] = parsedLine{}
	}
	s.lines, s.bytes = s.lines[:0], 0
}

// parallel runs fn(0..n-1) on up to workers goroutines, at least per items each.
func parallel(workers, n, per int, fn func(i int)) {
	if n == 0 {
		return
	}
	workers = min(workers, max(1, n/per))
	if workers == 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// wireMsg is the union of every message type's fields (PROTOCOL.md 3), so a message is
// decoded once. Chunk coordinates arrive as integers in X and Y.
type wireMsg struct {
	Type    string              `json:"type"`
	S       string              `json:"s"`
	F       string              `json:"f"`
	X       *float64            `json:"x"`
	Y       *float64            `json:"y"`
	D       *string             `json:"d"`
	Tick    *int64              `json:"tick"`
	M       moveEntries         `json:"m"`
	J       string              `json:"j"`
	Trace   string              `json:"trace"`
	N       int64               `json:"n"`
	U       int64               `json:"u"`
	Icon    string              `json:"icon"`
	Text    string              `json:"text"`
	R       string              `json:"r"`
	Q       string              `json:"q"`
	Name    string              `json:"name"`
	Dir     uint8               `json:"dir"`
	List    []core.SurfaceEntry `json:"list"`
	Created bool                `json:"created"`
	Counts  map[string]int64    `json:"counts"`
	Op      string              `json:"op"`
	Phase   string              `json:"phase"`
	Level   string              `json:"level"`
	Info    *core.MapInfo       `json:"info"`
	err     error
	// dRaw is a chunk's chart text when the fast decoder read the message (D is then
	// nil); hasD says the message had a chart. px is the decoded chart (in the window's
	// slab) or pxErr why it did not decode, set for chunk messages with a chart of an
	// acceptable length.
	dRaw  []byte
	hasD  bool
	px    *core.ChunkPixels
	pxErr error
}

// chart returns the message's chart text and whether it has one.
func (m *wireMsg) chart() ([]byte, bool) {
	if m.D != nil {
		return []byte(*m.D), true
	}
	return m.dRaw, m.hasD
}

// MaxMoveEntries bounds the entries of one move message (the capture sends at most 3,
// or 1 with carriages); a message with more is a bad message.
const MaxMoveEntries = 4096

// moveEntries decodes a move message's entries after counting them, so a hostile
// message cannot make ingest allocate for millions of entries.
type moveEntries []core.MoveEntry

func (m *moveEntries) UnmarshalJSON(b []byte) error {
	if i := skipWS(b, 0); i < len(b) && b[i] == '[' {
		if n := countArray(b[i:], MaxMoveEntries); n > MaxMoveEntries {
			return fmt.Errorf("move message with more than %d entries", MaxMoveEntries)
		}
	}
	return json.Unmarshal(b, (*[]core.MoveEntry)(m))
}

func knownType(t string) bool {
	switch t {
	case core.MsgChunk, core.MsgMove, core.MsgAdopt, core.MsgTag, core.MsgRecipe, core.MsgRail,
		core.MsgSurfaces, core.MsgResynced, core.MsgTrace, core.MsgMap:
		return true
	}
	return false
}

// parseEnvelope decodes the line's envelope header and finds its messages, which
// applySegment decodes a window at a time. It touches no shared state.
func (l *parsedLine) parseEnvelope() {
	if l.tooLong {
		return
	}
	line := bytes.TrimSpace(l.raw)
	if len(line) == 0 {
		l.blank = true
		return
	}
	env := &l.env
	in := internPool.Get().(*interner)
	msgs, fast := parseEnvelopeFast(line, env, in)
	internPool.Put(in)
	if !fast {
		*env = envHeader{}
		if err := json.Unmarshal(line, env); err != nil {
			l.err = fmt.Errorf("not an envelope: %w", err)
			return
		}
		// The line is valid JSON, so the scanner can find the messages without allocating.
		msgs = findMember(line, "msgs")
	}
	switch {
	case len(msgs) == 0 || string(msgs) == "null":
		msgs = nil
	case msgs[0] != '[':
		l.err = errors.New("not an envelope: msgs is not an array")
	}
	switch {
	case l.err != nil:
	case env.V != core.ProtocolVersion:
		l.err = fmt.Errorf("protocol version %d, want %d", env.V, core.ProtocolVersion)
	case env.Src == "" || env.Map == "" || env.RecvMs == 0:
		l.err = errors.New("envelope without src, map or recv_ms")
	case len(env.Src) > 512 || len(env.Map) > core.MaxNameBytes:
		l.err = errors.New("src or map too long")
	case env.Speed < 0 || env.Speed > 1000:
		l.err = fmt.Errorf("bad speed %v", env.Speed)
	}
	if l.err != nil || msgs == nil {
		return
	}
	l.it = newArrayIter(msgs)
}

var internPool = sync.Pool{New: func() any { return &interner{} }}

// decodeMsg decodes one message of at most maxBytes bytes: the hot types with the fast
// decoder, anything else (or anything it does not accept) with encoding/json.
func decodeMsg(raw []byte, maxBytes int) wireMsg {
	if len(raw) > maxBytes {
		return wireMsg{err: fmt.Errorf("message of %d bytes, max %d", len(raw), maxBytes)}
	}
	var m wireMsg
	in := internPool.Get().(*interner)
	fast := decodeMsgFast(raw, &m, in)
	internPool.Put(in)
	if fast {
		return m
	}
	m = wireMsg{}
	if err := json.Unmarshal(raw, &m); err != nil {
		var h core.MsgHeader
		if json.Unmarshal(raw, &h) == nil && !knownType(h.Type) {
			return wireMsg{Type: h.Type} // unknown types are ignored, whatever their fields
		}
		return wireMsg{Type: h.Type, err: err}
	}
	return m
}

// readLine reads one line (without its newline) into buf. A line longer than max is
// skipped up to its newline and reported as tooLong.
func readLine(br *bufio.Reader, buf []byte, max int) ([]byte, bool, error) {
	tooLong := false
	for {
		frag, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(frag) > max+1 { // +1 for the newline
				tooLong = true
				buf = buf[:0]
			} else {
				buf = append(buf, frag...)
			}
		}
		switch {
		case err == nil:
			if n := len(buf); n > 0 && buf[n-1] == '\n' {
				buf = buf[:n-1]
			}
			return buf, tooLong, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return buf, tooLong, err
		}
	}
}

// WallClock is the default map clock: the wall clock in milliseconds.
func WallClock() core.TimeMs { return core.TimeMs(nowMs()) }

// batch collects the decoded values of one body between flushes.
type batch struct {
	in     *Ingester
	ap     *applier
	resp   core.IngestResponse
	chunks []core.ChunkUpdate
	// raw are the move samples as decoded; jops the journey operations (moves, adopts,
	// resyncs) in message order, which the applier runs through the journey rules.
	raw    []core.MoverSample
	jops   []jop
	events []core.OverlayEvent
	// w and marks are, since the last flush, the newest message time per (source, map)
	// and the last finished envelope per source, ended the sources that ended
	// (Sources.Applied once applied).
	w     map[srcMap]core.TimeMs
	marks map[string]seqMark
	ended map[string]bool
	// slab holds the pixels of the queued chunk updates (FlushChunks entries; nslab used).
	slab  *[]core.ChunkPixels
	nslab int
	// moves is scratch space for one move message.
	moves []core.MoverSample
	// cur is the envelope whose messages applySegment applies.
	cur lineState
}

// lineState is the envelope being applied: its header, clock anchor, the bound its
// message times are clamped to (now) and the newest time of a chunk update or mover
// sample (the data with a horizon) queued since the last flush (maxT, valid when hasT;
// held once the line registered its map with Sources).
type lineState struct {
	ok     bool
	env    *envHeader
	anchor *movers.Anchor
	now    core.TimeMs
	maxT   core.TimeMs
	hasT   bool
	held   bool
}

// jop is one journey operation for the applier.
type jop struct {
	kind        uint8
	mapKey, src string
	id          string      // adopt: the journey
	t           core.TimeMs // resynced: the time
	n           int         // move: how many of the batch's raw samples
}

const (
	jopMove = iota
	jopAdopt
	jopBegin
	jopResynced
)

func (b *batch) full() bool {
	o := &b.in.opt
	return len(b.chunks) >= o.FlushChunks || len(b.raw) >= o.FlushSamples || len(b.events) >= o.FlushEvents
}

// foldTime moves the current line's newest message time into the batch's per-source
// times.
func (b *batch) foldTime() {
	c := &b.cur
	if !c.ok || !c.hasT {
		return
	}
	k := srcMap{c.env.Src, c.env.Map}
	if b.w == nil {
		b.w = map[srcMap]core.TimeMs{}
	}
	if cur, ok := b.w[k]; !ok || c.maxT > cur {
		b.w[k] = c.maxT
	}
	c.hasT = false
}

// finishLine records the end of the current line: its messages are all queued, so once
// the batch holding them is applied it counts as applied (Sources).
func (b *batch) finishLine() {
	c := &b.cur
	if !c.ok {
		return
	}
	b.foldTime()
	if c.env.Seq != nil {
		if b.marks == nil {
			b.marks = map[string]seqMark{}
		}
		mk := seqMark{*c.env.Seq, c.env.RecvMs}
		if cur, ok := b.marks[c.env.Src]; !ok || mk.seq > cur.seq || mk.recv > cur.recv {
			b.marks[c.env.Src] = mk
		}
	}
	c.ok = false
}

// slabs recycles the pixel slabs of applied batches (the stores copy the pixels). A
// short list rather than a sync.Pool, which the frequent collections of a busy ingest
// would empty.
var slabs = make(chan *[]core.ChunkPixels, 2)

func getSlab(n int) *[]core.ChunkPixels {
	select {
	case p := <-slabs:
		if len(*p) == n {
			return p
		}
	default:
	}
	s := make([]core.ChunkPixels, n)
	return &s
}

func putSlab(p *[]core.ChunkPixels) {
	select {
	case slabs <- p:
	default:
	}
}

// nextPixels returns the next free entry of the batch's slab.
func (b *batch) nextPixels() *core.ChunkPixels {
	if b.slab == nil {
		b.slab, b.nslab = getSlab(b.in.opt.FlushChunks), 0
	}
	px := &(*b.slab)[b.nslab]
	b.nslab++
	return px
}

func (b *batch) addError(err error) {
	if len(b.resp.Errors) < 10 {
		b.resp.Errors = append(b.resp.Errors, err.Error())
	}
}

func (b *batch) badLine(line int, err error) {
	b.resp.BadLines++
	b.in.stats.badLines.Add(1)
	b.addError(fmt.Errorf("line %d: %w", line, err))
}

func (b *batch) badMessage(env *envHeader, i int, err error) {
	b.in.stats.badMessages.Add(1)
	b.addError(fmt.Errorf("src %q tick %d message %d: %w", env.Src, env.Tick, i, err))
}

// beginLine starts applying one parsed line: it counts bad lines, and for an envelope
// skips it as a duplicate (a retry of an envelope already applied), or updates the data
// clock and the source's clock anchor. A caught_up (or ended) envelope without messages
// only records that its source has nothing older pending (and that it stopped).
func (b *batch) beginLine(l *parsedLine) {
	b.finishLine()
	b.cur = lineState{}
	switch {
	case l.tooLong:
		b.badLine(l.no, fmt.Errorf("line longer than %d bytes", b.in.opt.MaxLine))
		return
	case l.err != nil:
		b.badLine(l.no, l.err)
		return
	case l.blank:
		return
	}
	env := &l.env
	in := b.in
	in.stats.envelopes.Add(1)
	b.resp.Envelopes++
	if env.Seq != nil && (b.resp.Seq == nil || *env.Seq > *b.resp.Seq) {
		s := *env.Seq
		b.resp.Seq = &s
	}
	srcs := in.opt.Sources
	if srcs != nil && env.Seq != nil && srcs.Duplicate(env.Src, *env.Seq, env.RecvMs) {
		in.stats.duplicates.Add(1)
		b.resp.Duplicates++
		return
	}
	if (env.CaughtUp || env.Ended) && l.it.empty() {
		in.stats.caughtUp.Add(1)
		if srcs != nil {
			srcs.Seen(env.Src, env.Map, env.RecvMs)
		}
		if env.Ended {
			if b.ended == nil {
				b.ended = map[string]bool{}
			}
			b.ended[env.Src] = true
		}
		b.cur = lineState{ok: true, env: env, maxT: env.RecvMs, hasT: true}
		return
	}
	if a := in.anchors.Get(env.Src); a != nil {
		if tick, recv, ok := a.Last(); ok && recv == env.RecvMs && float64(env.Tick-tick) > 2*60*max(env.Speed, 1) {
			if in.stats.oneRecv.Add(1) == 1 || b.resp.OneRecv == 0 {
				in.opt.Logf("ingest: %s sends envelopes %d ticks apart with one recv_ms %d: a forwarder that lost the time (their samples collide)", env.Src, env.Tick-tick, env.RecvMs)
			}
			b.resp.OneRecv++
		}
	}
	wall := WallClock()
	if in.opt.DataClock != nil && in.opt.DataClock.Advance(env.RecvMs, wall, in.opt.MaxAhead, in.opt.MaxDataJump) {
		in.stats.clamped.Add(1)
	}
	now := in.opt.Clock()
	if in.opt.DataClock != nil {
		// Message times are clamped against the envelope's own receive time (bounded by
		// the wall clock), not the data clock, which moves at most MaxDataJump an
		// envelope: after a gap in the data the first envelopes keep their times.
		now = max(in.opt.DataClock.Seen(), min(env.RecvMs, wall+max(in.opt.MaxAhead, 0)))
	}
	anchor := in.anchors.Observe(env.Src, env.RecvMs, env.Tick, env.Speed)
	b.cur = lineState{ok: true, env: env, anchor: anchor, now: now}
	if srcs != nil {
		srcs.Heard(env.Src)
	}
}

// holds records the time of a chunk update or mover sample of the current envelope: the
// data the source holds its map's horizon for (Sources). The first one of a line
// registers the map, so a source's first data holds it before it is applied.
func (b *batch) holds(t core.TimeMs) {
	c := &b.cur
	if !c.hasT || t > c.maxT {
		c.maxT, c.hasT = t, true
	}
	if !c.held {
		c.held = true
		if srcs := b.in.opt.Sources; srcs != nil {
			srcs.Seen(c.env.Src, c.env.Map, t)
		}
	}
}

// timeOf returns the map time of a tick of the current envelope (its own tick when nil),
// clamped to MaxAhead after the server clock.
func (b *batch) timeOf(tick *int64) core.TimeMs {
	c := &b.cur
	tk := c.env.Tick
	if tick != nil {
		tk = *tick
	}
	t := c.anchor.T(tk)
	if ahead := b.in.opt.MaxAhead; ahead > 0 && t > c.now+ahead {
		b.in.stats.clamped.Add(1)
		t = c.now
	}
	return t
}

// applyMsg queues message i of the current envelope, updating the journeys.
func (b *batch) applyMsg(i int, m *wireMsg) {
	env := b.cur.env
	in := b.in
	if m.err != nil {
		b.badMessage(env, i, m.err)
		return
	}
	var err error
	switch m.Type {
	case core.MsgChunk:
		err = b.chunk(env, m)
	case core.MsgMove:
		err = b.move(env, m)
	case core.MsgAdopt:
		if m.J == "" {
			err = errors.New("adopt without j")
		} else {
			b.jops = append(b.jops, jop{kind: jopAdopt, mapKey: env.Map, src: env.Src, id: m.J})
		}
	case core.MsgTag, core.MsgRecipe, core.MsgRail:
		err = b.overlay(env, m, b.timeOf(nil))
	case core.MsgSurfaces:
		if !m.Created {
			// A resync starts by listing the surfaces (capture.resync): journeys the
			// source does not re-send before "resynced" are ended there.
			b.jops = append(b.jops, jop{kind: jopBegin, mapKey: env.Map, src: env.Src})
		}
	case core.MsgResynced:
		t := b.timeOf(nil)
		b.holds(t)
		b.jops = append(b.jops, jop{kind: jopResynced, mapKey: env.Map, src: env.Src, t: t})
		in.opt.Logf("ingest: %s resynced %q %v", env.Src, m.Trace, m.Counts)
	case core.MsgTrace:
		in.opt.Logf("ingest: %s trace %s %s %s", env.Src, m.Level, m.Op, m.Phase)
	case core.MsgMap:
		if m.Info == nil {
			err = errors.New("map message without info")
		} else {
			err = b.mapInfo(env, *m.Info)
		}
	default:
		// Unknown types are ignored (forward compatibility).
	}
	if err != nil {
		b.badMessage(env, i, err)
	}
}

// journeys returns the journey tracker of a map. Callers hold in.jmu.
func (in *Ingester) journeysOf(mapKey string) *movers.Journeys {
	in.jtick++
	if mj := in.journeys[mapKey]; mj != nil {
		mj.used = in.jtick
		return mj.js
	}
	if len(in.journeys) >= in.opt.MaxMaps {
		var oldK string
		var old uint64 = ^uint64(0)
		for k, mj := range in.journeys {
			if mj.used < old {
				oldK, old = k, mj.used
			}
		}
		delete(in.journeys, oldK)
	}
	mj := &mapJourneys{js: movers.NewJourneys(in.opt.MaxJourneys), used: in.jtick}
	if lm, ok := in.st.Movers.(interface {
		LastMeta(core.SurfaceKey, string) (core.MoverMeta, bool)
	}); ok {
		mj.js.Lookup = lm.LastMeta
	}
	in.journeys[mapKey] = mj
	return mj.js
}

func (b *batch) chunk(env *envHeader, m *wireMsg) error {
	ds := core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}
	if err := ds.Validate(); err != nil {
		return err
	}
	if m.X == nil || m.Y == nil {
		return errors.New("chunk without x or y")
	}
	cx, cy := *m.X, *m.Y
	if cx != math.Trunc(cx) || cy != math.Trunc(cy) || math.Abs(cx) > 1<<30 || math.Abs(cy) > 1<<30 {
		return fmt.Errorf("bad chunk coordinates %v,%v", cx, cy)
	}
	u := core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(cx), Y: int32(cy)}, T: b.timeOf(m.Tick)}
	b.holds(u.T)
	if d, ok := m.chart(); !ok {
		u.Deleted = true
	} else {
		if len(d) == 0 || len(d) > maxChart {
			return fmt.Errorf("chunk %v: chart of %d bytes", u.Chunk, len(d))
		}
		if m.px == nil {
			// The chart did not decode: an invalid chunk update.
			err := m.pxErr
			if err == nil {
				err = errors.New("chart not decoded")
			}
			b.resp.Chunks.Invalid++
			b.in.stats.chunks.invalid.Add(1)
			b.in.stats.badMessages.Add(1)
			b.addError(fmt.Errorf("chunk %v: %w", u.Chunk, err))
			return nil
		}
		px := b.nextPixels()
		*px = *m.px
		u.Pixels = px
	}
	b.chunks = append(b.chunks, u)
	return nil
}

func (b *batch) move(env *envHeader, m *wireMsg) error {
	sfc := core.SurfaceKey{Map: env.Map, Surface: m.S}
	if err := sfc.Validate(); err != nil {
		return err
	}
	n0 := len(b.raw)
	for _, e := range m.M {
		if e.J == "" {
			continue
		}
		tick := env.Tick
		if e.Tick != nil {
			tick = *e.Tick
		}
		s := core.MoverSample{Surface: sfc, T: b.timeOf(e.Tick), Tick: tick}
		b.holds(s.T)
		s.Meta.ID = e.J
		if e.Gone {
			s.Flags = core.FlagGone
			b.raw = append(b.raw, s)
			continue
		}
		s.Meta = core.MoverMeta{ID: e.J, Kind: core.MoverKind(e.K), Force: e.F, Name: e.Name, Carriages: e.N, Kinds: e.O, Color: e.C}
		s.X, s.Y = e.X, e.Y
		if e.H != nil {
			s.Orientation = *e.H
		} else {
			s.Flags |= core.FlagNoHeading
		}
		if e.Stop {
			s.Flags |= core.FlagStop
		}
		if e.ES {
			s.Flags |= core.FlagEndSwitch
		}
		if len(e.Car) > 0 {
			s.Carriages = make([]core.Carriage, len(e.Car))
			for i, c := range e.Car {
				s.Carriages[i] = core.Carriage{X: c[0], Y: c[1], Orientation: c[2]}
			}
		}
		if e.V != nil && s.Meta.Kind == core.KindTrain && e.H != nil {
			// A rail path sample (docs/v3/RAIL-GEOMETRY.md 8). Pieces that are not valid
			// make the path unknown here: the viewer starts a new rail path at the sample.
			s.Flags |= core.FlagRail
			s.Speed = *e.V
			s.RailStart = e.RU
			if len(e.RP) > 0 {
				s.Rail = make([]core.RailPiece, 0, min(len(e.RP), core.MaxRailPieces))
				for _, p := range e.RP {
					q := core.RailPiece{X: int32(p[0]), Y: int32(p[1]), Dir: uint8(p[2]), Kind: uint8(p[3])}
					if len(s.Rail) == core.MaxRailPieces || int64(q.X) != p[0] || int64(q.Y) != p[1] ||
						int64(q.Dir) != p[2] || int64(q.Kind) != p[3] || !q.Valid() {
						s.Rail, s.RailStart = nil, true
						break
					}
					s.Rail = append(s.Rail, q)
				}
			}
		}
		b.raw = append(b.raw, s)
	}
	if n := len(b.raw) - n0; n > 0 {
		b.jops = append(b.jops, jop{kind: jopMove, mapKey: env.Map, src: env.Src, n: n})
	}
	return nil
}

func (b *batch) overlay(env *envHeader, m *wireMsg, t core.TimeMs) error {
	ev := core.OverlayEvent{T: t, DS: core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}}
	if err := ev.DS.Validate(); err != nil {
		return err
	}
	iv := &ev.Interval
	iv.Instance = env.Instance
	inst := strconv.FormatInt(env.Instance, 10)
	open := m.X != nil && m.Y != nil
	switch m.Type {
	case core.MsgTag:
		iv.Kind, iv.Key = core.OverlayTag, inst+"/"+strconv.FormatInt(m.N, 10)
		if open {
			iv.Icon, iv.Text = m.Icon, m.Text
		}
	case core.MsgRecipe:
		iv.Kind, iv.Key = core.OverlayRecipe, inst+"/"+strconv.FormatInt(m.U, 10)
		open = open && m.R != ""
		if open {
			iv.Recipe, iv.Quality = m.R, m.Q
		}
	case core.MsgRail:
		iv.Kind, iv.Key = core.OverlayRail, inst+"/"+strconv.FormatInt(m.U, 10)
		open = open && m.Name != ""
		if open {
			iv.Name, iv.Direction = m.Name, m.Dir
		}
	}
	if open {
		iv.X, iv.Y = *m.X, *m.Y
	} else {
		ev.Close = true
	}
	b.events = append(b.events, ev)
	return nil
}

func (b *batch) mapInfo(env *envHeader, info core.MapInfo) error {
	if info.Key == "" {
		info.Key = env.Map
	}
	if info.Key != env.Map {
		return fmt.Errorf("map message for %q in an envelope of map %q", info.Key, env.Map)
	}
	maps := b.in.st.Maps
	if maps == nil {
		return nil
	}
	changed, err := maps.Put(info)
	if err != nil {
		return err
	}
	b.in.stats.maps.Add(1)
	if changed && b.in.opt.Notifier != nil {
		b.in.opt.Notifier.MapsChanged()
	}
	return nil
}

// flush hands everything queued to the applier. It fails when the applier failed on an
// earlier batch (the reader then stops).
func (b *batch) flush() error {
	if b.ap.failed.Load() {
		return errApplierFailed
	}
	var job applyJob
	if len(b.chunks) > 0 {
		job.updates, b.chunks = b.chunks, nil
		job.slab, b.slab = b.slab, nil
	}
	job.raw, b.raw = b.raw, nil
	job.jops, b.jops = b.jops, nil
	job.events, b.events = b.events, nil
	b.foldTime()
	job.w, b.w = b.w, nil
	job.marks, b.marks = b.marks, nil
	job.ended, b.ended = b.ended, nil
	if len(job.updates)+len(job.jops)+len(job.events)+len(job.w)+len(job.marks)+len(job.ended) == 0 {
		return nil
	}
	if dc := b.in.opt.DataClock; dc != nil {
		job.clock = dc.Seen()
	}
	b.ap.jobs <- job
	return nil
}

var errApplierFailed = errors.New("ingest: an earlier batch failed")

// applyJob is one batch from the reader to the applier.
type applyJob struct {
	updates []core.ChunkUpdate
	raw     []core.MoverSample
	jops    []jop
	events  []core.OverlayEvent
	// w, marks and ended go to Sources.Applied once the batch is applied.
	w     map[srcMap]core.TimeMs
	marks map[string]seqMark
	ended map[string]bool
	// clock is the data clock's seen value when the batch was handed over, published
	// once the batch is applied.
	clock core.TimeMs
	// slab holds the updates' pixels; recycled once applied.
	slab *[]core.ChunkPixels
}

// applier applies batches to the stores in order on its own goroutine, so the stores'
// work overlaps reading, parsing and chart decoding of the next batch. At most one batch
// waits while another is applied.
type applier struct {
	in     *Ingester
	ctx    context.Context
	jobs   chan applyJob
	done   chan struct{}
	failed atomic.Bool
	err    error // set by the applier goroutine; read after done
	res    struct{ chunks, samples, overlays core.ApplyResult }
}

func (in *Ingester) startApplier(ctx context.Context) *applier {
	a := &applier{in: in, ctx: ctx, jobs: make(chan applyJob, 1), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for job := range a.jobs {
			if a.err != nil {
				continue // drain after a failure
			}
			if err := a.apply(&job); err != nil {
				a.err = err
				a.failed.Store(true)
			} else {
				if dc := in.opt.DataClock; dc != nil {
					dc.Publish(job.clock)
				}
				if in.opt.Sources != nil {
					in.opt.Sources.Applied(job.w, job.marks)
					for src := range job.ended {
						in.opt.Sources.End(src)
					}
				}
			}
			if job.slab != nil {
				putSlab(job.slab)
			}
		}
	}()
	return a
}

// finish waits for the queued batches and returns the first failure.
func (a *applier) finish() error {
	close(a.jobs)
	<-a.done
	return a.err
}

// apply applies one batch: chunk updates, overlay events, then the journey operations
// and the mover store's Append. When Append fails the journey operations are rolled back,
// so the retried batch meets the journeys as this one did.
func (a *applier) apply(job *applyJob) error {
	in, ctx := a.in, a.ctx
	if len(job.updates) > 0 {
		if in.st.Terrain == nil {
			a.res.chunks.Invalid += len(job.updates)
		} else {
			res, err := in.st.Terrain.Apply(ctx, job.updates)
			if err != nil {
				if errors.Is(err, core.ErrOverloaded) {
					in.stats.overloaded.Add(1)
				}
				return fmt.Errorf("terrain: %w", err)
			}
			a.res.chunks.Add(res)
			in.stats.chunks.add(res)
			if in.opt.Notifier != nil {
				in.opt.Notifier.ChunksApplied(job.updates)
			}
		}
	}
	if len(job.events) > 0 {
		if in.st.Overlays == nil {
			a.res.overlays.Invalid += len(job.events)
		} else {
			res, err := in.st.Overlays.Apply(ctx, job.events)
			if err != nil {
				return fmt.Errorf("overlays: %w", err)
			}
			a.res.overlays.Add(res)
			in.stats.overlays.add(res)
		}
	}
	if len(job.jops) == 0 {
		return nil
	}
	in.jmu.Lock()
	defer in.jmu.Unlock()
	var marked []*movers.Journeys
	// The output is sized at once (it does not grow by copies) and the raw samples are
	// let go once converted.
	samples := make([]core.MoverSample, 0, len(job.raw)+len(job.raw)/8)
	raw := job.raw
	job.raw = nil
	for _, op := range job.jops {
		js := in.journeysOf(op.mapKey)
		if !slices.Contains(marked, js) {
			js.Mark()
			marked = append(marked, js)
		}
		switch op.kind {
		case jopMove:
			samples = js.Move(op.src, raw[:op.n], samples)
			raw = raw[op.n:]
		case jopAdopt:
			js.Adopt(op.src, op.id)
		case jopBegin:
			js.Begin(op.src)
		case jopResynced:
			samples = js.Resynced(op.src, op.t, samples)
		}
	}
	raw = nil
	commit := func(ok bool) {
		for _, js := range marked {
			if ok {
				js.Commit()
			} else {
				js.Rollback()
			}
		}
	}
	if len(samples) == 0 {
		commit(true)
		return nil
	}
	if in.st.Movers == nil {
		a.res.samples.Invalid += len(samples)
		commit(true)
		return nil
	}
	res, err := in.st.Movers.Append(ctx, samples)
	if err != nil {
		commit(false)
		if errors.Is(err, core.ErrOverloaded) {
			in.stats.overloaded.Add(1)
		}
		return fmt.Errorf("movers: %w", err)
	}
	commit(true)
	a.res.samples.Add(res)
	in.stats.samples.add(res)
	if in.opt.Notifier != nil {
		in.opt.Notifier.SamplesApplied(samples)
	}
	return nil
}
