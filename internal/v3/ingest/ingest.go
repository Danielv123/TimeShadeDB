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
	// (updates it counted as late or invalid included; the store does not say which).
	ChunksApplied(updates []core.ChunkUpdate)
	// SamplesApplied is called with the mover samples of a batch the mover store took.
	SamplesApplied(samples []core.MoverSample)
	// MapsChanged is called when maps.json changed.
	MapsChanged()
}

// Options tune an Ingester. Zero values select the defaults.
type Options struct {
	// MaxLine bounds one NDJSON line (default 4 MiB, API.md 5); longer lines are bad lines.
	MaxLine int
	// FlushChunks, FlushSamples and FlushEvents bound the decoded values held before
	// they are applied to the stores (defaults 2,048 chunk updates, 16,384 samples and
	// 16,384 overlay events), which bounds ingest memory per request whatever the body.
	FlushChunks, FlushSamples, FlushEvents int
	// MaxAhead: a message time more than this after the server clock is clamped to the
	// clock (default 10 s; < 0 disables). A broken anchor or game clock would otherwise
	// hold terrain open windows and mover open rows far in the future.
	MaxAhead core.TimeMs
	// Clock returns the server's map clock (default: wall clock).
	Clock func() core.TimeMs
	// DataClock, if set, observes every valid envelope's recv_ms before its messages are
	// timed (pass DataClock.Now as Clock too, for a server replaying recorded captures).
	DataClock *DataClock
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
}

// Ingester applies capture envelopes to the stores. It is safe for concurrent use;
// bodies are applied one at a time, so the messages of one source keep their order.
type Ingester struct {
	st  Stores
	opt Options

	mu       sync.Mutex
	anchors  *movers.Anchors
	journeys map[string]*mapJourneys
	jtick    uint64

	stats struct {
		envelopes, bytes, badLines, badMessages, clamped, maps, overloaded atomic.Int64
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
	if opt.FlushChunks <= 0 {
		opt.FlushChunks = 2048
	}
	if opt.FlushSamples <= 0 {
		opt.FlushSamples = 16384
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
		Chunks:      in.stats.chunks.get(),
		Samples:     in.stats.samples.get(),
		Overlays:    in.stats.overlays.get(),
	}
	in.mu.Lock()
	for _, mj := range in.journeys {
		js := mj.js.Stats()
		s.OwnerDrops += js.OwnerDrops
		s.UnknownGones += js.UnknownGones
		s.SurfaceChanges += js.SurfaceChanges
		s.Quarantined += js.Quarantined
	}
	in.mu.Unlock()
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
// wrapping core.ErrOverloaded when a store refused a batch for backpressure (API.md 5:
// HTTP 503); the batches before it were applied, and retrying the whole body is safe
// because ingest is idempotent (terrain updates are absolute, samples dedupe on
// (journey, t), unchanged overlay events are no-ops). Other errors are store or read
// failures.
func (in *Ingester) Ingest(ctx context.Context, body io.Reader) (core.IngestResponse, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	b := &batch{in: in, ap: in.startApplier(ctx)}
	b.resp.Errors = []string{}
	err := b.read(ctx, body)
	if err == nil {
		err = b.flush()
	}
	// The applier finishes the batches already handed over; its error wins, because
	// the reader only stops early when the applier failed.
	aerr := b.ap.finish()
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
	seg := &segment{}
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
		}
		if seg.full() || (eof && len(seg.lines) > 0) {
			// Parse the segment's lines in parallel (no shared state), then apply them in
			// order.
			seg.parse(in.opt.Workers)
			for i := range seg.lines {
				b.apply(&seg.lines[i])
				if b.full() {
					if err := b.flush(); err != nil {
						return err
					}
				}
			}
			seg.reset()
		}
		if eof {
			return nil
		}
	}
}

// segment holds raw lines of a body that are parsed together.
type segment struct {
	lines []parsedLine
	bytes int
	pool  [][]byte // line buffers for reuse
}

// Segment bounds: lines parse in parallel in groups of up to this many lines or bytes.
const (
	segmentLines = 1024
	segmentBytes = 8 << 20
)

// parsedLine is one body line and, after parse, its envelope.
type parsedLine struct {
	no      int
	raw     []byte
	tooLong bool
	env     core.Envelope
	msgs    []wireMsg
	err     error // the line is not a valid envelope
}

func (s *segment) next() []byte {
	if n := len(s.pool); n > 0 {
		b := s.pool[n-1][:0]
		s.pool = s.pool[:n-1]
		return b
	}
	return make([]byte, 0, 4096)
}

func (s *segment) add(no int, line []byte, tooLong bool) {
	s.lines = append(s.lines, parsedLine{no: no, raw: line, tooLong: tooLong})
	s.bytes += len(line)
}

func (s *segment) full() bool { return len(s.lines) >= segmentLines || s.bytes >= segmentBytes }

func (s *segment) reset() {
	for i := range s.lines {
		if cap(s.lines[i].raw) <= 1<<20 {
			s.pool = append(s.pool, s.lines[i].raw)
		}
		s.lines[i] = parsedLine{}
	}
	s.lines, s.bytes = s.lines[:0], 0
}

// parse decodes the segment in two parallel passes: the envelopes of every line, then
// every message of every line (forwarders may pack hundreds of messages in one line).
func (s *segment) parse(workers int) {
	parallel(workers, len(s.lines), 1, func(i int) { s.lines[i].parseEnvelope() })
	type ref struct{ line, msg int }
	var refs []ref
	for i := range s.lines {
		l := &s.lines[i]
		for j := range l.msgs {
			refs = append(refs, ref{i, j})
		}
	}
	parallel(workers, len(refs), 64, func(k int) {
		r := refs[k]
		l := &s.lines[r.line]
		l.msgs[r.msg] = decodeMsg(l.env.Msgs[r.msg])
	})
	for i := range s.lines {
		s.lines[i].env.Msgs = nil
	}
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
	M       []core.MoveEntry    `json:"m"`
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
}

func knownType(t string) bool {
	switch t {
	case core.MsgChunk, core.MsgMove, core.MsgAdopt, core.MsgTag, core.MsgRecipe, core.MsgRail,
		core.MsgSurfaces, core.MsgResynced, core.MsgTrace, core.MsgMap:
		return true
	}
	return false
}

// parseEnvelope decodes the line's envelope and sizes its messages; the messages are
// decoded by decodeMsg. It touches no shared state.
func (l *parsedLine) parseEnvelope() {
	if l.tooLong {
		return
	}
	line := bytes.TrimSpace(l.raw)
	if len(line) == 0 {
		return
	}
	env := &l.env
	if err := json.Unmarshal(line, env); err != nil {
		l.err = fmt.Errorf("not an envelope: %w", err)
		return
	}
	switch {
	case env.V != core.ProtocolVersion:
		l.err = fmt.Errorf("protocol version %d, want %d", env.V, core.ProtocolVersion)
	case env.Src == "" || env.Map == "" || env.RecvMs == 0:
		l.err = errors.New("envelope without src, map or recv_ms")
	case len(env.Src) > 512 || len(env.Map) > core.MaxNameBytes:
		l.err = errors.New("src or map too long")
	case env.Speed < 0 || env.Speed > 1000:
		l.err = fmt.Errorf("bad speed %v", env.Speed)
	}
	if l.err != nil {
		return
	}
	l.msgs = make([]wireMsg, len(env.Msgs))
}

// decodeMsg decodes one message.
func decodeMsg(raw json.RawMessage) wireMsg {
	var m wireMsg
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

// pendingChunk is a chunk message waiting for its chart to be decoded at flush.
type pendingChunk struct {
	u core.ChunkUpdate
	d string
}

// batch collects the decoded values of one body between flushes.
type batch struct {
	in      *Ingester
	ap      *applier
	resp    core.IngestResponse
	chunks  []pendingChunk
	samples []core.MoverSample
	events  []core.OverlayEvent
	// moves is scratch space for one move message.
	moves []core.MoverSample
}

func (b *batch) full() bool {
	o := &b.in.opt
	return len(b.chunks) >= o.FlushChunks || len(b.samples) >= o.FlushSamples || len(b.events) >= o.FlushEvents
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

func (b *batch) badMessage(env *core.Envelope, i int, err error) {
	b.in.stats.badMessages.Add(1)
	b.addError(fmt.Errorf("src %q tick %d message %d: %w", env.Src, env.Tick, i, err))
}

// apply queues one parsed line's messages, updating the anchors and journeys.
func (b *batch) apply(l *parsedLine) {
	switch {
	case l.tooLong:
		b.badLine(l.no, fmt.Errorf("line longer than %d bytes", b.in.opt.MaxLine))
		return
	case l.err != nil:
		b.badLine(l.no, l.err)
		return
	case l.msgs == nil && l.env.V == 0:
		return // blank line
	}
	env := &l.env
	in := b.in
	in.stats.envelopes.Add(1)
	b.resp.Envelopes++
	if env.Seq != nil && (b.resp.Seq == nil || *env.Seq > *b.resp.Seq) {
		s := *env.Seq
		b.resp.Seq = &s
	}
	if in.opt.DataClock != nil {
		in.opt.DataClock.Observe(env.RecvMs)
	}
	anchor := in.anchors.Observe(env.Src, env.RecvMs, env.Tick, env.Speed)
	now := in.opt.Clock()
	timeOf := func(tick *int64) core.TimeMs {
		tk := env.Tick
		if tick != nil {
			tk = *tick
		}
		t := anchor.T(tk)
		if in.opt.MaxAhead > 0 && t > now+in.opt.MaxAhead {
			in.stats.clamped.Add(1)
			t = now
		}
		return t
	}
	for i := range l.msgs {
		m := &l.msgs[i]
		if m.err != nil {
			b.badMessage(env, i, m.err)
			continue
		}
		var err error
		switch m.Type {
		case core.MsgChunk:
			err = b.chunk(env, m, timeOf)
		case core.MsgMove:
			err = b.move(env, m, timeOf)
		case core.MsgAdopt:
			if m.J == "" {
				err = errors.New("adopt without j")
			} else {
				b.journeys(env.Map).Adopt(env.Src, m.J)
			}
		case core.MsgTag, core.MsgRecipe, core.MsgRail:
			err = b.overlay(env, m, timeOf(nil))
		case core.MsgSurfaces:
			if !m.Created {
				// A resync starts by listing the surfaces (capture.resync): journeys the
				// source does not re-send before "resynced" are ended there.
				b.journeys(env.Map).Begin(env.Src)
			}
		case core.MsgResynced:
			b.samples = b.journeys(env.Map).Resynced(env.Src, timeOf(nil), b.samples)
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

}

func (b *batch) journeys(mapKey string) *movers.Journeys {
	in := b.in
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
	in.journeys[mapKey] = mj
	return mj.js
}

func (b *batch) chunk(env *core.Envelope, m *wireMsg, timeOf func(*int64) core.TimeMs) error {
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
	pc := pendingChunk{u: core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(cx), Y: int32(cy)}, T: timeOf(m.Tick)}}
	if m.D == nil {
		pc.u.Deleted = true
	} else {
		if len(*m.D) == 0 || len(*m.D) > 64<<10 {
			return fmt.Errorf("chunk %v: chart of %d bytes", pc.u.Chunk, len(*m.D))
		}
		pc.d = *m.D
	}
	b.chunks = append(b.chunks, pc)
	return nil
}

func (b *batch) move(env *core.Envelope, m *wireMsg, timeOf func(*int64) core.TimeMs) error {
	sfc := core.SurfaceKey{Map: env.Map, Surface: m.S}
	if err := sfc.Validate(); err != nil {
		return err
	}
	b.moves = b.moves[:0]
	for _, e := range m.M {
		if e.J == "" {
			continue
		}
		tick := env.Tick
		if e.Tick != nil {
			tick = *e.Tick
		}
		s := core.MoverSample{Surface: sfc, T: timeOf(e.Tick), Tick: tick}
		s.Meta.ID = e.J
		if e.Gone {
			s.Flags = core.FlagGone
			b.moves = append(b.moves, s)
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
		b.moves = append(b.moves, s)
	}
	b.samples = b.journeys(env.Map).Move(env.Src, b.moves, b.samples)
	return nil
}

func (b *batch) overlay(env *core.Envelope, m *wireMsg, t core.TimeMs) error {
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

func (b *batch) mapInfo(env *core.Envelope, info core.MapInfo) error {
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

// flush decodes the queued charts and hands everything queued to the applier. It fails
// when the applier failed on an earlier batch (the reader then stops).
func (b *batch) flush() error {
	if b.ap.failed.Load() {
		return errApplierFailed
	}
	var job applyJob
	if len(b.chunks) > 0 {
		job.updates = b.decodeCharts()
		b.chunks = b.chunks[:0]
	}
	job.samples, b.samples = b.samples, nil
	job.events, b.events = b.events, nil
	if len(job.updates)+len(job.samples)+len(job.events) == 0 {
		return nil
	}
	b.ap.jobs <- job
	return nil
}

var errApplierFailed = errors.New("ingest: an earlier batch failed")

// applyJob is one batch from the reader to the applier.
type applyJob struct {
	updates []core.ChunkUpdate
	samples []core.MoverSample
	events  []core.OverlayEvent
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
			if err := a.apply(job); err != nil {
				a.err = err
				a.failed.Store(true)
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

func (a *applier) apply(job applyJob) error {
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
	if len(job.samples) > 0 {
		if in.st.Movers == nil {
			a.res.samples.Invalid += len(job.samples)
		} else {
			res, err := in.st.Movers.Append(ctx, job.samples)
			if err != nil {
				if errors.Is(err, core.ErrOverloaded) {
					in.stats.overloaded.Add(1)
				}
				return fmt.Errorf("movers: %w", err)
			}
			a.res.samples.Add(res)
			in.stats.samples.add(res)
			if in.opt.Notifier != nil {
				in.opt.Notifier.SamplesApplied(job.samples)
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
	return nil
}

// decodeCharts inflates the queued charts in parallel. Charts that do not decode are
// counted as invalid chunk updates and reported.
func (b *batch) decodeCharts() []core.ChunkUpdate {
	pcs := b.chunks
	errs := make([]error, len(pcs))
	// One slab for the batch's pixels; the terrain store copies them.
	slab := make([]core.ChunkPixels, len(pcs))
	workers := min(b.in.opt.Workers, max(1, len(pcs)/64))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(pcs) {
					return
				}
				if pcs[i].u.Deleted {
					continue
				}
				errs[i] = decodeChart(pcs[i].d, &slab[i])
			}
		}()
	}
	wg.Wait()
	out := make([]core.ChunkUpdate, 0, len(pcs))
	for i := range pcs {
		if errs[i] != nil {
			b.resp.Chunks.Invalid++
			b.in.stats.chunks.invalid.Add(1)
			b.in.stats.badMessages.Add(1)
			b.addError(fmt.Errorf("chunk %v: %w", pcs[i].u.Chunk, errs[i]))
			continue
		}
		u := pcs[i].u
		u.Pixels = &slab[i]
		out = append(out, u)
	}
	return out
}
