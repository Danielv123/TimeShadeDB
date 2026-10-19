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
	// MaxSources bounds the clock anchors (default 4,096).
	MaxSources int
	// MaxJourneys bounds the tracked journeys per map (default 100,000), MaxMaps the maps
	// with a journey tracker (default 256; the least recently used is forgotten).
	MaxJourneys, MaxMaps int
	// Workers decode charts in parallel (default GOMAXPROCS).
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
		opt.Workers = runtime.GOMAXPROCS(0)
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
	b := &batch{in: in}
	resp := &b.resp
	resp.Errors = []string{}
	br := bufio.NewReaderSize(body, 64<<10)
	line := make([]byte, 0, 64<<10)
	lineNo := 0
	for {
		if err := ctx.Err(); err != nil {
			return *resp, err
		}
		var tooLong bool
		var err error
		line, tooLong, err = readLine(br, line[:0], in.opt.MaxLine)
		if err != nil && !errors.Is(err, io.EOF) {
			return *resp, err
		}
		eof := errors.Is(err, io.EOF)
		if len(line) > 0 || tooLong {
			lineNo++
			in.stats.bytes.Add(int64(len(line)))
			if tooLong {
				b.badLine(lineNo, fmt.Errorf("line longer than %d bytes", in.opt.MaxLine))
			} else if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				if err := b.envelope(trimmed); err != nil {
					b.badLine(lineNo, err)
				}
				if b.full() {
					if err := b.flush(ctx); err != nil {
						return *resp, err
					}
				}
			}
		}
		if eof {
			break
		}
	}
	if err := b.flush(ctx); err != nil {
		return *resp, err
	}
	return *resp, nil
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

// envelope decodes one line and queues its messages.
func (b *batch) envelope(line []byte) error {
	var env core.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("not an envelope: %w", err)
	}
	if env.V != core.ProtocolVersion {
		return fmt.Errorf("protocol version %d, want %d", env.V, core.ProtocolVersion)
	}
	if env.Src == "" || env.Map == "" || env.RecvMs == 0 {
		return errors.New("envelope without src, map or recv_ms")
	}
	if len(env.Src) > 512 || len(env.Map) > core.MaxNameBytes {
		return errors.New("src or map too long")
	}
	if env.Speed < 0 || env.Speed > 1000 {
		return fmt.Errorf("bad speed %v", env.Speed)
	}
	in := b.in
	in.stats.envelopes.Add(1)
	b.resp.Envelopes++
	if env.Seq != nil && (b.resp.Seq == nil || *env.Seq > *b.resp.Seq) {
		s := *env.Seq
		b.resp.Seq = &s
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
	for i, raw := range env.Msgs {
		var h core.MsgHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			b.badMessage(&env, i, err)
			continue
		}
		var err error
		switch h.Type {
		case core.MsgChunk:
			err = b.chunk(&env, raw, timeOf)
		case core.MsgMove:
			err = b.move(&env, raw, timeOf)
		case core.MsgAdopt:
			var m core.AdoptMsg
			if err = json.Unmarshal(raw, &m); err == nil {
				if m.J == "" {
					err = errors.New("adopt without j")
				} else {
					b.journeys(env.Map).Adopt(env.Src, m.J)
				}
			}
		case core.MsgTag, core.MsgRecipe, core.MsgRail:
			err = b.overlay(&env, h.Type, raw, timeOf(nil))
		case core.MsgSurfaces:
			var m core.SurfacesMsg
			if err = json.Unmarshal(raw, &m); err == nil && !m.Created {
				// A resync starts by listing the surfaces (capture.resync): journeys the
				// source does not re-send before "resynced" are ended there.
				b.journeys(env.Map).Begin(env.Src)
			}
		case core.MsgResynced:
			var m core.ResyncedMsg
			if err = json.Unmarshal(raw, &m); err == nil {
				b.samples = b.journeys(env.Map).Resynced(env.Src, timeOf(nil), b.samples)
				in.opt.Logf("ingest: %s resynced %q %v", env.Src, m.Trace, m.Counts)
			}
		case core.MsgTrace:
			var m core.TraceMsg
			if err = json.Unmarshal(raw, &m); err == nil {
				in.opt.Logf("ingest: %s trace %s %s %s", env.Src, m.Level, m.Op, m.Phase)
			}
		case core.MsgMap:
			var m core.MapMsg
			if err = json.Unmarshal(raw, &m); err == nil {
				err = b.mapInfo(&env, m.Info)
			}
		default:
			// Unknown types are ignored (forward compatibility).
		}
		if err != nil {
			b.badMessage(&env, i, err)
		}
	}
	return nil
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

func (b *batch) chunk(env *core.Envelope, raw json.RawMessage, timeOf func(*int64) core.TimeMs) error {
	var m core.ChunkMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	ds := core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}
	if err := ds.Validate(); err != nil {
		return err
	}
	pc := pendingChunk{u: core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: m.X, Y: m.Y}, T: timeOf(m.Tick)}}
	if m.D == nil {
		pc.u.Deleted = true
	} else {
		if len(*m.D) == 0 || len(*m.D) > 64<<10 {
			return fmt.Errorf("chunk %d,%d: chart of %d bytes", m.X, m.Y, len(*m.D))
		}
		pc.d = *m.D
	}
	b.chunks = append(b.chunks, pc)
	return nil
}

func (b *batch) move(env *core.Envelope, raw json.RawMessage, timeOf func(*int64) core.TimeMs) error {
	var m core.MoveMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
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

func (b *batch) overlay(env *core.Envelope, typ string, raw json.RawMessage, t core.TimeMs) error {
	ev := core.OverlayEvent{T: t}
	ev.Interval.Instance = env.Instance
	inst := strconv.FormatInt(env.Instance, 10)
	switch typ {
	case core.MsgTag:
		var m core.TagMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		ev.DS = core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}
		ev.Interval.Kind, ev.Interval.Key = core.OverlayTag, inst+"/"+strconv.FormatInt(m.N, 10)
		if m.X == nil || m.Y == nil {
			ev.Close = true
		} else {
			ev.Interval.X, ev.Interval.Y, ev.Interval.Icon, ev.Interval.Text = *m.X, *m.Y, m.Icon, m.Text
		}
	case core.MsgRecipe:
		var m core.RecipeMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		ev.DS = core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}
		ev.Interval.Kind, ev.Interval.Key = core.OverlayRecipe, inst+"/"+strconv.FormatInt(m.U, 10)
		if m.R == "" || m.X == nil || m.Y == nil {
			ev.Close = true
		} else {
			ev.Interval.X, ev.Interval.Y, ev.Interval.Recipe, ev.Interval.Quality = *m.X, *m.Y, m.R, m.Q
		}
	case core.MsgRail:
		var m core.RailMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		ev.DS = core.DatastoreKey{Map: env.Map, Surface: m.S, Force: m.F}
		ev.Interval.Kind, ev.Interval.Key = core.OverlayRail, inst+"/"+strconv.FormatInt(m.U, 10)
		if m.Name == "" || m.X == nil || m.Y == nil {
			ev.Close = true
		} else {
			ev.Interval.X, ev.Interval.Y, ev.Interval.Name, ev.Interval.Direction = *m.X, *m.Y, m.Name, m.Dir
		}
	}
	if err := ev.DS.Validate(); err != nil {
		return err
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

// flush decodes the queued charts and applies everything queued to the stores.
func (b *batch) flush(ctx context.Context) error {
	in := b.in
	if len(b.chunks) > 0 {
		updates := b.decodeCharts()
		b.chunks = b.chunks[:0]
		if len(updates) > 0 {
			if in.st.Terrain == nil {
				b.resp.Chunks.Invalid += len(updates)
			} else {
				res, err := in.st.Terrain.Apply(ctx, updates)
				if err != nil {
					if errors.Is(err, core.ErrOverloaded) {
						in.stats.overloaded.Add(1)
					}
					return fmt.Errorf("terrain: %w", err)
				}
				b.resp.Chunks.Add(res)
				in.stats.chunks.add(res)
				if in.opt.Notifier != nil {
					in.opt.Notifier.ChunksApplied(updates)
				}
			}
		}
	}
	if len(b.samples) > 0 {
		if in.st.Movers == nil {
			b.resp.Samples.Invalid += len(b.samples)
		} else {
			res, err := in.st.Movers.Append(ctx, b.samples)
			if err != nil {
				if errors.Is(err, core.ErrOverloaded) {
					in.stats.overloaded.Add(1)
				}
				return fmt.Errorf("movers: %w", err)
			}
			b.resp.Samples.Add(res)
			in.stats.samples.add(res)
			if in.opt.Notifier != nil {
				in.opt.Notifier.SamplesApplied(b.samples)
			}
		}
		b.samples = b.samples[:0]
	}
	if len(b.events) > 0 {
		if in.st.Overlays == nil {
			b.resp.Overlays.Invalid += len(b.events)
		} else {
			res, err := in.st.Overlays.Apply(ctx, b.events)
			if err != nil {
				return fmt.Errorf("overlays: %w", err)
			}
			b.resp.Overlays.Add(res)
			in.stats.overlays.add(res)
		}
		b.events = b.events[:0]
	}
	return nil
}

// decodeCharts inflates the queued charts in parallel. Charts that do not decode are
// counted as invalid chunk updates and reported.
func (b *batch) decodeCharts() []core.ChunkUpdate {
	pcs := b.chunks
	errs := make([]error, len(pcs))
	pixels := make([]*core.ChunkPixels, len(pcs))
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
				pixels[i], errs[i] = core.DecodeChart(pcs[i].d)
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
		u.Pixels = pixels[i]
		out = append(out, u)
	}
	return out
}
