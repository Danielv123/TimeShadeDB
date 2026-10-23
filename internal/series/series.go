package series

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"

	"timeshadedb/internal/core"
)

// Series is a handle to one open series. It implements core.Series. A handle is safe
// for one writer and concurrent readers; Close releases it (the store may keep the
// series open for later handles).
type Series struct {
	f      *file
	s      *Store
	closed atomic.Bool
}

var _ core.Series = (*Series)(nil)

func (h *Series) file() (*file, error) {
	if h.closed.Load() {
		return nil, core.ErrClosed
	}
	return h.f, nil
}

// Name returns the series name.
func (h *Series) Name() string { return h.f.name }

// Append implements core.Series. The record is written to .blk but not fsynced; call
// Sync to make it durable and write its index record.
func (h *Series) Append(ctx context.Context, rec core.Record) (uint64, error) {
	f, err := h.file()
	if err != nil {
		return 0, err
	}
	return f.append(rec)
}

// Len implements core.Series. It is 0 after the handle or the store was closed.
func (h *Series) Len() uint64 {
	if h.closed.Load() {
		return 0
	}
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	return uint64(len(h.f.entries))
}

// Meta implements core.Series.
func (h *Series) Meta(seq uint64) (core.RecordMeta, error) {
	if h.closed.Load() {
		return core.RecordMeta{}, core.ErrClosed
	}
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	if h.f.blk == nil {
		return core.RecordMeta{}, core.ErrClosed
	}
	if seq >= uint64(len(h.f.entries)) {
		return core.RecordMeta{}, fmt.Errorf("%w: record %d", core.ErrNoVersion, seq)
	}
	return h.f.entries[seq].meta(seq), nil
}

// Read implements core.Series.
func (h *Series) Read(ctx context.Context, seq uint64) (core.Record, error) {
	f, err := h.file()
	if err != nil {
		return core.Record{}, err
	}
	recs, err := f.readRange(seq, seq)
	if err != nil {
		return core.Record{}, err
	}
	return recs[0], nil
}

// ReadRange returns records first..last (inclusive) with one contiguous read. The
// payloads share one fresh buffer owned by the caller.
func (h *Series) ReadRange(ctx context.Context, first, last uint64) ([]core.Record, error) {
	f, err := h.file()
	if err != nil {
		return nil, err
	}
	return f.readRange(first, last)
}

// Chain implements core.Series.
func (h *Series) Chain(ctx context.Context, t core.TimeMs) ([]core.Record, error) {
	f, err := h.file()
	if err != nil {
		return nil, err
	}
	f.mu.RLock()
	end := endAt(f.entries, t)
	if end == 0 {
		f.mu.RUnlock()
		return nil, nil
	}
	start := baseAtOrBefore(f.entries, end-1)
	f.mu.RUnlock()
	return f.readRange(uint64(start), uint64(end-1))
}

// Sync implements core.Series.
func (h *Series) Sync() error {
	f, err := h.file()
	if err != nil {
		return err
	}
	return f.sync()
}

// Close implements core.Series. It releases the handle; the data stays on disk.
func (h *Series) Close() error {
	if h.closed.Swap(true) {
		return nil
	}
	if h.s != nil {
		h.s.release(h.f)
	}
	return nil
}

// The helpers below serve the terrain store. A delta record (core.RecordDelta) is one
// tile version: version v is reached by the v-th delta record.

// Versions returns the number of delta records.
func (h *Series) Versions() uint32 {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	if n := len(h.f.entries); n > 0 {
		return h.f.entries[n-1].deltas
	}
	return 0
}

// VersionAt returns the number of delta records with TEnd <= t.
func (h *Series) VersionAt(t core.TimeMs) uint32 {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	if end := endAt(h.f.entries, t); end > 0 {
		return h.f.entries[end-1].deltas
	}
	return 0
}

// VersionSeq returns the seq of the delta record that reaches version v (v >= 1).
func (h *Series) VersionSeq(v uint32) (uint64, bool) {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	e := h.f.entries
	i := sort.Search(len(e), func(i int) bool { return e[i].deltas >= v })
	if v == 0 || i == len(e) {
		return 0, false
	}
	return uint64(i), true
}

// VersionMeta returns the metadata of the delta record that reaches version v.
func (h *Series) VersionMeta(v uint32) (core.RecordMeta, bool) {
	seq, ok := h.VersionSeq(v)
	if !ok {
		return core.RecordMeta{}, false
	}
	m, err := h.Meta(seq)
	return m, err == nil
}

// Plan describes the records to read to rebuild version v: First..Last inclusive.
// First is a base record (keyframe or snapshot), or record 0, or (with Resume) the
// record after a cached version.
type Plan struct {
	First, Last uint64
	// FromBase is true when First is a base record (or record 0) and reading starts from
	// an empty state; false when it continues from version Resume.
	FromBase bool
	Resume   uint32
}

// PlanVersion returns the read plan for version v (v >= 1). If the delta reaching v is
// followed by a keyframe with the same effective time, the plan is that keyframe
// alone. have, if not nil, reports whether an earlier version is available in a cache;
// it is asked for versions newer than the chain's base, newest first, at most
// maxProbe times.
func (h *Series) PlanVersion(v uint32, have func(uint32) bool, maxProbe int) (Plan, bool) {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	e := h.f.entries
	i := sort.Search(len(e), func(i int) bool { return e[i].deltas >= v })
	if v == 0 || i == len(e) {
		return Plan{}, false
	}
	if i+1 < len(e) && e[i+1].kind == core.RecordKeyframe && e[i+1].tEnd() == e[i].tEnd() && e[i+1].deltas == v {
		return Plan{First: uint64(i + 1), Last: uint64(i + 1), FromBase: true}, true
	}
	base := baseAtOrBefore(e, i)
	var baseV uint32
	if e[base].kind.IsBase() {
		baseV = e[base].deltas
	}
	if have != nil {
		for pv := v - 1; pv > baseV && maxProbe > 0; pv-- {
			maxProbe--
			if have(pv) {
				// Records after the delta reaching pv, up to i. A keyframe among them is
				// a full state; applying it in order is still correct.
				j := sort.Search(len(e), func(j int) bool { return e[j].deltas >= pv })
				return Plan{First: uint64(j + 1), Last: uint64(i), Resume: pv}, true
			}
		}
	}
	return Plan{First: uint64(base), Last: uint64(i), FromBase: true}, true
}

// History returns the versions whose delta records have TEnd in [t0, t1), at most
// version max, oldest first.
func (h *Series) History(t0, t1 core.TimeMs, max uint32) []core.VersionPoint {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	e := h.f.entries
	i := sort.Search(len(e), func(i int) bool { return e[i].tEnd() >= t0 })
	var out []core.VersionPoint
	for ; i < len(e) && e[i].tEnd() < t1; i++ {
		if e[i].kind == core.RecordDelta && e[i].deltas <= max {
			out = append(out, core.VersionPoint{T: e[i].tEnd(), Version: core.Version(e[i].deltas)})
		}
	}
	return out
}

// Summary describes a series for the terrain store's per-tile state.
type Summary struct {
	Records  uint64
	Versions uint32
	// FirstT and LastT are the TEnd of the first and last delta records.
	FirstT, LastT core.TimeMs
	// LastBaseLen is the payload length of the newest base record (0 if none), and
	// SinceBase / BytesSinceBase count the records and payload bytes after it (after
	// record 0 if there is no base).
	LastBaseLen    int
	FirstLen       int // payload length of record 0
	SinceBase      int
	BytesSinceBase int64
	// Size is the .blk size.
	Size int64
}

// Summary scans the index.
func (h *Series) Summary() Summary {
	h.f.mu.RLock()
	defer h.f.mu.RUnlock()
	e := h.f.entries
	var s Summary
	s.Records = uint64(len(e))
	s.Size = h.f.size
	if len(e) > 0 {
		s.FirstLen = int(e[0].plen)
	}
	for i := range e {
		if e[i].kind == core.RecordDelta {
			if s.Versions == 0 {
				s.FirstT = e[i].tEnd()
			}
			s.Versions = e[i].deltas
			s.LastT = e[i].tEnd()
		}
	}
	for i := len(e) - 1; i >= 0; i-- {
		if e[i].kind.IsBase() {
			s.LastBaseLen = int(e[i].plen)
			break
		}
		s.SinceBase++
		s.BytesSinceBase += int64(e[i].plen)
	}
	return s
}
