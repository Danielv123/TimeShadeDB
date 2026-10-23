package minimap

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
)

// The mover import streams: each surface's window files are decoded in name (time)
// order with a small lookahead, every fix works on a bounded tail of each journey, and
// the surfaces are merged by time into the store. Memory is bounded by the number of
// live journeys and openRunLag of rows, not by the length of the history.
//
// The watermark M of a surface is the name of the next window file to read: a window
// file's rows are at or after its name, so every row before M has been read. Rows of a
// journey before M are complete and in their final order.
const (
	// openRunLag bounds how long a row waits for the row after it. A journey's rows are
	// fixed in segments that end at a row without a floor stamp (such a row is never
	// changed and separates the fixes before it from those after it). Floor-stamped rows
	// with no such row after them are fixed without it once the watermark is this far
	// past them. Every fix only looks a few seconds ahead, so this is not reached on the
	// test data except where a journey pauses.
	openRunLag = int64(30 * core.Minute)
	// seedMaxLag: a keyframe seed is placed at its sample time, but no earlier than this
	// before its keyframe, so seeds never fall behind the rows already released.
	seedMaxLag = openRunLag
	// outChunk is the number of samples a surface hands to the merge at once.
	outChunk = 4096
)

// isJump is research/scripts/jumps.py: consecutive samples with 0 < dt <= 3 s whose
// displacement exceeds max(|v|, |v_prev|) * dt + 6 tiles.
func isJump(p, r *srcRow) bool {
	dt := float64(r.t-p.t) / 1000
	if dt <= 0 || dt > 3 {
		return false
	}
	return dist(p, r) > max(speed(r), speed(p))*dt+6
}

// isEndSwitch reports whether train sample r, following sample p, is an end switch: a
// jump at low speed whose length matches the train's front-to-back distance
// ((carriages - 1) * 7 tiles, +-30% + 2 tiles), within endSwitchMaxDt.
func isEndSwitch(p, r *srcRow) bool {
	n := r.meta.Carriages
	dt := float64(r.t-p.t) / 1000
	if n < 2 || dt <= 0 || dt > endSwitchMaxDt || min(speed(r), speed(p)) >= 3 {
		return false
	}
	d := dist(p, r)
	if d <= max(speed(r), speed(p))*dt+6 {
		return false
	}
	l := float64((n - 1) * carriagePitch)
	return math.Abs(d-l) <= 0.3*l+2
}

// Universal Edges crossings without a gone row. A train that enters an edge runs on into
// a buffer past the edge line (about 32 tiles on the test data) and stops there; the
// instance on the other side places it near the line, so its next sample lies behind the
// last one while it heads back towards it. The capture normally sends a gone for the
// handoff; where it did not, the importer adds one (MoverReport.CrossingsInferred).
// Trains only: a vehicle can turn around between two samples.
const (
	// crossingBand: both samples lie within this many tiles of the same edge line.
	crossingBand = 64
	// crossingMinJump: the backwards step is at least this long (tiles).
	crossingMinJump = 8
)

// nearEdge reports whether (x, y) lies within crossingBand of the axis-aligned (or any)
// edge segment e, measured from the segment.
func nearEdge(e core.Quad, x, y float64) bool {
	x1, y1, x2, y2 := e[0], e[1], e[2], e[3]
	dx, dy := x2-x1, y2-y1
	l2 := dx*dx + dy*dy
	u := 0.0
	if l2 > 0 {
		u = math.Max(0, math.Min(1, ((x-x1)*dx+(y-y1)*dy)/l2))
	}
	return math.Hypot(x-(x1+u*dx), y-(y1+u*dy)) <= crossingBand
}

// isCrossing reports whether sample r, following sample p of the same journey with no
// gone between them, is an edge crossing without a gone: r lies at least
// crossingMinJump behind p (not ahead of p's own motion) and moves towards it, and both
// lie near the same edge. An end switch moves away from the old head, and a train cannot
// get behind its old position while heading towards it without driving past it, which
// the capture would have sent. r's direction is its velocity, or when it reports none (a
// train placed at rest), the step to next (the row after it, nil when unknown).
func isCrossing(edges []core.Quad, p, r, next *srcRow) bool {
	if len(edges) == 0 {
		return false
	}
	dx, dy := r.x-p.x, r.y-p.y
	if math.Hypot(dx, dy) < crossingMinJump || p.vx*dx+p.vy*dy > 0 {
		return false // a step ahead of a moving p is not behind it
	}
	vx, vy := r.vx, r.vy
	if vx == 0 && vy == 0 && next != nil && next.flags&rowGone == 0 {
		vx, vy = next.x-r.x, next.y-r.y
		if math.Hypot(vx, vy) < 0.5 {
			return false
		}
	}
	if vx*dx+vy*dy >= 0 {
		return false
	}
	for _, e := range edges {
		if nearEdge(e, p.x, p.y) && nearEdge(e, r.x, r.y) {
			return true
		}
	}
	return false
}

// journey is the streaming state of one journey on one surface.
type journey struct {
	id     string
	srcOK  bool // has window rows (counted in Source.Journeys)
	active bool // listed in moverSurface.active
	// ctx is the last fixed row: the context the next segment's fixes read.
	ctx    srcRow
	hasCtx bool
	// pend are the rows after ctx, not fixed yet, sorted by stamp (stable).
	pend []srcRow
	// Source-order state: the previous source row, when it was a sample.
	srcPrev   srcRow
	srcPrevOK bool
	// Output state: the previous emitted window sample (end switches, jumps), gones
	// waiting for a handoff sample, and the last vehicle heading.
	outPrev   srcRow
	outPrevOK bool
	gones     []int64
	lastH     float64
	haveH     bool
	emitted   bool
}

// outSample is a fixed sample ready for the store, in compact form.
type outSample struct {
	t     int64
	x, y  float64
	o     float64
	flags core.MoverFlags
	meta  *core.MoverMeta
	seq   uint64
}

type outHeap []outSample

func (h outHeap) Len() int { return len(h) }
func (h outHeap) Less(i, j int) bool {
	if h[i].t != h[j].t {
		return h[i].t < h[j].t
	}
	return h[i].seq < h[j].seq
}
func (h outHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *outHeap) Push(x any)   { *h = append(*h, x.(outSample)) }
func (h *outHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// moverSurface fixes one surface's movers as a stream.
type moverSurface struct {
	key      core.SurfaceKey
	windowMs int64
	edges    []core.Quad // Universal Edges segments of the surface (crossings)
	rep      *MoverReport
	metas    map[core.MoverMeta]*core.MoverMeta
	journeys map[string]*journey
	active   []*journey // journeys with pending rows, in first-activation order
	out      outHeap    // fixed samples not yet released, by (t, seq)
	seq      uint64
	buf      []srcRow
	kf       int // next keyframe index
	// send hands a released chunk (sorted by t) to the merge.
	send func([]outSample) error
}

func newMoverSurface(key core.SurfaceKey, windowMs int64, rep *MoverReport) *moverSurface {
	return &moverSurface{key: key, windowMs: windowMs, rep: rep,
		metas: map[core.MoverMeta]*core.MoverMeta{}, journeys: map[string]*journey{}}
}

func newMoverReport(src *SourceSurface) *MoverReport {
	return &MoverReport{Surface: src.Key.ID(), WindowFiles: len(src.Windows), KeyframeFiles: len(src.Keyframes),
		Source: newCounts(), Imported: newCounts(), Retimed: map[string]int{}}
}

func (s *moverSurface) intern(m core.MoverMeta) *core.MoverMeta {
	p, ok := s.metas[m]
	if !ok {
		p = &m
		s.metas[m] = p
	}
	return p
}

func (s *moverSurface) journey(id string) *journey {
	j, ok := s.journeys[id]
	if !ok {
		j = &journey{id: id}
		s.journeys[id] = j
	}
	return j
}

// addWindow adds a decoded window file's rows, marking floor-stamped ones.
func (s *moverSurface) addWindow(dw *decodedWindow) {
	if dw.err != nil {
		s.rep.addErr(dw.err)
		return
	}
	s.rep.InvalidRows += dw.invalid
	local := make([]*core.MoverMeta, len(dw.metas))
	for i, m := range dw.metas {
		if dw.metaOK[i] {
			local[i] = s.intern(m)
		}
	}
	W := s.windowMs
	for k, r := range dw.rows {
		r.meta = local[dw.mi[k]]
		// The floor after a close is the close time + 1 (or later), and a clamped
		// sample is stamped with it, so it is the earliest time in its file.
		if mod := r.t % W; mod <= 2 || (r.t == dw.first && dw.first%W < boundarySlack) {
			r.flags |= rowBoundary
		}
		s.addRow(r)
	}
}

// addRow inserts a window row into its journey's pending rows (after equal stamps, so
// arrival order is kept).
func (s *moverSurface) addRow(r srcRow) {
	j := s.journey(r.meta.ID)
	if !j.srcOK {
		j.srcOK = true
		s.rep.Source.Journeys[string(r.meta.Kind)]++
	}
	p := j.pend
	if n := len(p); n == 0 || p[n-1].t <= r.t {
		j.pend = append(p, r)
	} else {
		i := sort.Search(n, func(i int) bool { return p[i].t > r.t })
		p = append(p, srcRow{})
		copy(p[i+1:], p[i:])
		p[i] = r
		j.pend = p
	}
	if !j.active {
		j.active = true
		s.active = append(s.active, j)
	}
}

// advance fixes and emits what the watermark M allows; final fixes everything.
func (s *moverSurface) advance(M int64, final bool) {
	keep := s.active[:0]
	for _, j := range s.active {
		s.advanceJourney(j, M, final)
		if len(j.pend) > 0 {
			keep = append(keep, j)
		} else {
			j.active = false
		}
	}
	clear(s.active[len(keep):])
	s.active = keep
}

// advanceJourney fixes the longest prefix of j's pending rows whose fixes are settled:
// up to the last row before M without a floor stamp, plus (when nothing settles them)
// floor-stamped rows more than openRunLag before M.
func (s *moverSurface) advanceJourney(j *journey, M int64, final bool) {
	cut := 0
	for k := range j.pend {
		if j.pend[k].t >= M {
			break
		}
		if j.pend[k].flags&rowBoundary == 0 {
			cut = k + 1
		}
	}
	switch {
	case final:
		cut = len(j.pend)
	default:
		lim := M - openRunLag
		forced := cut
		for forced < len(j.pend) && j.pend[forced].t < lim {
			forced++
		}
		if forced > cut {
			s.rep.OpenRunCuts++
			cut = forced
		}
	}
	if cut > 0 {
		s.fixSegment(j, cut)
	}
}

// fixSegment counts, fixes and emits j.pend[:cut].
func (s *moverSurface) fixSegment(j *journey, cut int) {
	rep := s.rep
	seg := j.pend[:cut]
	for k := range seg {
		j.sourceStep(&seg[k], rep)
	}
	buf := s.buf[:0]
	if j.hasCtx {
		buf = append(buf, j.ctx)
	}
	from := len(buf)
	buf = append(buf, seg...)
	retimeRows(buf, from, string(buf[from].meta.Kind), rep)
	// retimeRows keeps the order; the sort only guards the emission order.
	sort.SliceStable(buf[from:], func(a, b int) bool { return buf[from+a].t < buf[from+b].t })
	// The duplicate rule: a row is dropped when the next row of the journey has the same
	// time. The next row after the segment is still pending; it can only have this time
	// if its stamp equals it (re-timing never lands on an earlier row's time).
	hasNext := cut < len(j.pend)
	var next int64
	if hasNext {
		next = j.pend[cut].t
	}
	for k := from; k < len(buf); k++ {
		r := &buf[k]
		if (k+1 < len(buf) && buf[k+1].t == r.t) || (k+1 == len(buf) && hasNext && next == r.t) {
			if r.flags&rowGone != 0 {
				rep.DupGonesDropped++
			} else {
				rep.DupSamplesDropped++
			}
			continue
		}
		// The next row (when it is in this segment) gives r's direction for the edge
		// crossing test when r itself reports no velocity.
		var next *srcRow
		if k+1 < len(buf) {
			next = &buf[k+1]
		}
		s.emit(j, r, next)
	}
	j.ctx, j.hasCtx = buf[len(buf)-1], true
	n := copy(j.pend, j.pend[cut:])
	clear(j.pend[n:])
	j.pend = j.pend[:n]
	if cap(j.pend) > 1024 && n < cap(j.pend)/4 {
		j.pend = append([]srcRow(nil), j.pend...)
	}
	s.buf = buf
}

// sourceStep counts source row r (in the journey's time order, before any fix).
func (j *journey) sourceStep(r *srcRow, rep *MoverReport) {
	k := string(r.meta.Kind)
	if r.flags&rowGone != 0 {
		rep.Source.Gones[k]++
		j.srcPrevOK = false
		return
	}
	rep.Source.Samples[k]++
	if j.srcPrevOK {
		if j.srcPrev.t == r.t {
			rep.SourceDupPairs++
		}
		if r.meta.Kind == core.KindTrain && isJump(&j.srcPrev, r) {
			rep.TrainJumpsBefore++
		}
	}
	j.srcPrev, j.srcPrevOK = *r, true
}

// emit hands one fixed row to the output: end switches, handoff and jump counting,
// flags and headings. next is the journey's row after r when it is known (or nil).
func (s *moverSurface) emit(j *journey, r *srcRow, next *srcRow) {
	rep := s.rep
	kind := r.meta.Kind
	k := string(kind)
	if !j.emitted {
		j.emitted = true
		rep.Imported.Journeys[k]++
	}
	if rep.FirstT == 0 || core.TimeMs(r.t) < rep.FirstT {
		rep.FirstT = core.TimeMs(r.t)
	}
	rep.LastT = max(rep.LastT, core.TimeMs(r.t))
	o := outSample{t: r.t, meta: r.meta, flags: core.FlagNoTick}
	if r.flags&rowGone != 0 {
		rep.Imported.Gones[k]++
		j.gones = append(j.gones, r.t)
		j.outPrevOK = false
		o.flags |= core.FlagGone
		s.push(o)
		return
	}
	if r.flags&rowSeed == 0 && j.outPrevOK && kind == core.KindTrain &&
		r.t-j.outPrev.t >= 2 && isCrossing(s.edges, &j.outPrev, r, next) {
		// An edge crossing without a gone: add the gone just before r, so the store flags
		// the handoff and viewers bridge it instead of drawing a jump back.
		rep.CrossingsInferred++
		s.emit(j, &srcRow{t: r.t - 1, meta: r.meta, flags: rowGone}, nil)
	}
	rep.Imported.Samples[k]++
	for _, g := range j.gones {
		if core.TimeMs(r.t-g) <= core.HandoffWindow {
			rep.Handoffs++
		}
	}
	j.gones = j.gones[:0]
	// End switches and jumps compare window samples only (not keyframe seeds).
	if r.flags&rowSeed == 0 {
		if kind == core.KindTrain && j.outPrevOK {
			if isEndSwitch(&j.outPrev, r) {
				r.flags |= rowEndSwitch
				rep.EndSwitches++
			} else if isJump(&j.outPrev, r) {
				rep.TrainJumpsAfter++
			}
		}
		j.outPrev, j.outPrevOK = *r, true
	}
	o.x, o.y = r.x, r.y
	if r.flags&rowEndSwitch != 0 {
		o.flags |= core.FlagEndSwitch
	}
	if r.vx == 0 && r.vy == 0 {
		o.flags |= core.FlagStop
		rep.Stops++
	}
	switch {
	case r.flags&rowHasH != 0:
		o.o = r.h
	case kind == core.KindVehicle && (r.vx != 0 || r.vy != 0):
		j.lastH, j.haveH = headingFromVelocity(r.vx, r.vy), true
		o.o = j.lastH
		o.flags |= core.FlagNoHeading
		rep.HeadingsDerived++
		rep.NoHeading++
	case kind == core.KindVehicle && j.haveH:
		o.o = j.lastH
		o.flags |= core.FlagNoHeading
		rep.HeadingsDerived++
		rep.NoHeading++
	default:
		o.flags |= core.FlagNoHeading
		rep.NoHeading++
	}
	s.push(o)
}

func (s *moverSurface) push(o outSample) {
	o.seq = s.seq
	s.seq++
	heap.Push(&s.out, o)
}

// keyframes processes the keyframes before the watermark M. A keyframe mover whose
// journey has no window row at or before the keyframe is imported as a seed sample.
// Only the first keyframe that lists a journey can seed it.
func (s *moverSurface) keyframes(ctx context.Context, src *SourceSurface, M int64) error {
	rep := s.rep
	for s.kf < len(src.Keyframes) && src.Keyframes[s.kf] < M {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := src.Keyframes[s.kf]
		s.kf++
		path := src.KeyframePath(name)
		b, err := readGz(path, maxWindowBytes)
		if err != nil {
			rep.addErr(err)
			continue
		}
		var kf keyframeJSON
		if err := json.Unmarshal(b, &kf); err != nil {
			rep.addErr(fmt.Errorf("%s: %w: %v", path, ErrFormat, err))
			continue
		}
		if kf.T != name {
			rep.addErr(fmt.Errorf("%s: %w: keyframe time %d", path, ErrFormat, kf.T))
			continue
		}
		K := kf.T
		for _, lm := range kf.Movers {
			rep.KeyframeRows++
			row, meta, ok := parseLiveMover(lm)
			if !ok || row.t > K {
				rep.InvalidRows++
				continue
			}
			j := s.journeys[meta.ID]
			// The journey's last window row at or before K: the last pending one, or else
			// the last fixed one (fixed rows all lie before the previous watermark <= K).
			var last *srcRow
			if j != nil {
				if i := sort.Search(len(j.pend), func(i int) bool { return j.pend[i].t > K }); i > 0 {
					last = &j.pend[i-1]
				} else if j.hasCtx {
					last = &j.ctx
				}
			}
			if last != nil {
				if last.t == row.t {
					rep.KeyframeMatched++
				}
				if last.flags&rowGone != 0 {
					rep.KeyframeConflicts++
				}
				continue
			}
			if j != nil && j.emitted {
				continue // seeded by an earlier keyframe
			}
			if row.t < K-seedMaxLag {
				row.t = K - seedMaxLag
				rep.KeyframeSeedsClamped++
			}
			row.meta = s.intern(meta)
			row.flags |= rowSeed
			s.emit(s.journey(meta.ID), &row, nil)
			rep.KeyframeSeeded++
		}
	}
	return nil
}

// release hands every fixed sample at or before R to the merge, in time order.
func (s *moverSurface) release(R int64) error {
	var chunk []outSample
	for s.out.Len() > 0 && s.out[0].t <= R {
		if chunk == nil {
			chunk = make([]outSample, 0, min(outChunk, s.out.Len()))
		}
		chunk = append(chunk, heap.Pop(&s.out).(outSample))
		if len(chunk) == outChunk {
			if err := s.send(chunk); err != nil {
				return err
			}
			chunk = nil
		}
	}
	if len(chunk) > 0 {
		return s.send(chunk)
	}
	return nil
}

// step processes keyframes before M, fixes what M allows and releases what can no
// longer be preceded: every row still pending has a stamp >= M - openRunLag and moves
// at most retimeMaxShift earlier, and every later seed lies after M - seedMaxLag.
func (s *moverSurface) step(ctx context.Context, src *SourceSurface, M int64, final bool) error {
	if err := s.keyframes(ctx, src, M); err != nil {
		return err
	}
	s.advance(M, final)
	R := int64(math.MaxInt64)
	if !final {
		R = M - max(openRunLag+retimeMaxShift, seedMaxLag) - 1
	}
	return s.release(R)
}

// run streams the surface: futs yields the decoded window files in name order.
func (s *moverSurface) run(ctx context.Context, src *SourceSurface, futs <-chan chan decodedWindow) error {
	for _, name := range src.Windows {
		if err := s.step(ctx, src, name, false); err != nil {
			return err
		}
		var f chan decodedWindow
		var ok bool
		select {
		case f, ok = <-futs:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("minimap: window prefetch ended early")
		}
		var dw decodedWindow
		select {
		case dw = <-f:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.addWindow(&dw)
	}
	return s.step(ctx, src, math.MaxInt64, true)
}

// decodePool decodes window files on a fixed number of workers for every surface.
type decodePool struct {
	jobs    chan decodeJob
	workers sync.WaitGroup
	feeders sync.WaitGroup
}

type decodeJob struct {
	path string
	out  chan decodedWindow
}

func newDecodePool(workers int) *decodePool {
	p := &decodePool{jobs: make(chan decodeJob)}
	for i := 0; i < max(workers, 1); i++ {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for jb := range p.jobs {
				jb.out <- decodeWindowFile(jb.path)
			}
		}()
	}
	return p
}

// prefetch decodes src's window files in name order, at most lookahead files ahead of
// the reader. The futures arrive in file order; each yields one decodedWindow.
func (p *decodePool) prefetch(ctx context.Context, src *SourceSurface, lookahead int) <-chan chan decodedWindow {
	futs := make(chan chan decodedWindow, max(lookahead, 1))
	p.feeders.Add(1)
	go func() {
		defer p.feeders.Done()
		defer close(futs)
		for _, name := range src.Windows {
			f := make(chan decodedWindow, 1)
			select {
			case p.jobs <- decodeJob{path: src.WindowPath(name), out: f}:
			case <-ctx.Done():
				return
			}
			select {
			case futs <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return futs
}

// close waits for the feeders (their context must be done or their files read), then
// stops the workers.
func (p *decodePool) close() {
	p.feeders.Wait()
	close(p.jobs)
	p.workers.Wait()
}

// moverStream is one surface's released output as seen by the merge.
type moverStream struct {
	key core.SurfaceKey
	ch  chan []outSample
	cur []outSample
}

// streamHeap orders surfaces by their next sample's time, then by surface order.
type streamHeap struct {
	idx     []int
	streams []*moverStream
}

func (h *streamHeap) Len() int { return len(h.idx) }
func (h *streamHeap) Less(i, j int) bool {
	a, b := h.streams[h.idx[i]], h.streams[h.idx[j]]
	if a.cur[0].t != b.cur[0].t {
		return a.cur[0].t < b.cur[0].t
	}
	return h.idx[i] < h.idx[j]
}
func (h *streamHeap) Swap(i, j int) { h.idx[i], h.idx[j] = h.idx[j], h.idx[i] }
func (h *streamHeap) Push(x any)    { h.idx = append(h.idx, x.(int)) }
func (h *streamHeap) Pop() any {
	x := h.idx[len(h.idx)-1]
	h.idx = h.idx[:len(h.idx)-1]
	return x
}

// next loads the stream's next non-empty chunk; false when the surface is done.
func (st *moverStream) next() bool {
	for c := range st.ch {
		if len(c) > 0 {
			st.cur = c
			return true
		}
	}
	st.cur = nil
	return false
}

// importMovers streams every surface's movers through the fixes and feeds them to the
// store in global time order. Each Commit trails the feed by more than
// core.HandoffWindow, so the store sees the sample that makes a gone a handoff before it
// closes the gone's window.
func importMovers(ctx context.Context, d *Dataset, opt *Options, store core.MoverStore) ([]*MoverReport, core.ApplyResult, error) {
	var res core.ApplyResult
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var srcs []*SourceSurface
	for _, src := range d.Surfaces {
		if opt.wantSurface(src.Key) {
			srcs = append(srcs, src)
		}
	}
	reports := make([]*MoverReport, len(srcs))
	streams := make([]*moverStream, len(srcs))
	errs := make([]error, len(srcs))
	pool := newDecodePool(opt.Workers)
	var wg sync.WaitGroup
	for i, src := range srcs {
		reports[i] = newMoverReport(src)
		st := &moverStream{key: src.Key, ch: make(chan []outSample, 2)}
		streams[i] = st
		s := newMoverSurface(src.Key, d.WindowMs, reports[i])
		s.edges = d.edges(src.Key)
		s.send = func(c []outSample) error {
			select {
			case st.ch <- c:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		futs := pool.prefetch(ctx, src, opt.Workers)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(st.ch)
			errs[i] = s.run(ctx, src, futs)
		}()
	}
	done := func(err error) ([]*MoverReport, core.ApplyResult, error) {
		cancel()
		wg.Wait()
		pool.close()
		if err == nil {
			err = errors.Join(errs...)
		}
		return reports, res, err
	}

	h := &streamHeap{streams: streams}
	for i, st := range streams {
		if st.next() {
			h.idx = append(h.idx, i)
		}
	}
	heap.Init(h)

	batch := make([]core.MoverSample, 0, 8192)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		r, err := store.Append(ctx, batch)
		res.Add(r)
		batch = batch[:0]
		if err == nil {
			err = ctx.Err()
		}
		return err
	}
	every := opt.MoverCommitEvery
	var next, last core.TimeMs
	started := false
	for h.Len() > 0 {
		st := streams[h.idx[0]]
		o := st.cur[0]
		st.cur = st.cur[1:]
		if len(st.cur) > 0 || st.next() {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
		}
		t := core.TimeMs(o.t)
		if !started {
			next, started = core.WindowStart(t, core.MoverWindowLen)+every, true
		}
		for t > next+core.HandoffWindow {
			if err := flush(); err != nil {
				return done(err)
			}
			if err := store.Commit(ctx, next); err != nil {
				return done(err)
			}
			next += every
			if t > next+core.HandoffWindow+every {
				next = core.WindowStart(t-core.HandoffWindow, core.MoverWindowLen)
			}
		}
		batch = append(batch, core.MoverSample{
			Surface:     st.key,
			Meta:        *o.meta,
			T:           t,
			X:           o.x,
			Y:           o.y,
			Orientation: o.o,
			Flags:       o.flags,
		})
		last = t
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return done(err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return done(err)
	}
	if err := flush(); err != nil {
		return done(err)
	}
	if started {
		if err := store.Commit(ctx, core.WindowEnd(last, core.MoverWindowLen)); err != nil {
			return done(err)
		}
	}
	return done(nil)
}
