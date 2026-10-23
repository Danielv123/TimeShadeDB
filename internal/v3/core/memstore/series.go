package memstore

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"timeshadedb/internal/v3/core"
)

// SeriesStore is the reference core.SeriesStore.
type SeriesStore struct {
	mu     sync.Mutex
	closed bool
	series map[string]*Series
}

var _ core.SeriesStore = (*SeriesStore)(nil)

// Series is the reference core.Series.
type Series struct {
	mu     sync.RWMutex
	closed bool
	recs   []core.Record
}

var _ core.Series = (*Series)(nil)

// NewSeriesStore returns an empty store.
func NewSeriesStore() *SeriesStore {
	return &SeriesStore{series: map[string]*Series{}}
}

var seriesPart = regexp.MustCompile(`^[A-Za-z0-9_~.-]+$`)

// ValidSeriesName reports whether name is a slash-separated path of id-safe parts with
// no empty, "." or ".." part.
func ValidSeriesName(name string) bool {
	if name == "" {
		return false
	}
	for _, p := range strings.Split(name, "/") {
		if p == "." || p == ".." || !seriesPart.MatchString(p) {
			return false
		}
	}
	return true
}

// Open implements core.SeriesStore.
func (s *SeriesStore) Open(ctx context.Context, name string, create bool) (core.Series, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, core.ErrClosed
	}
	if !ValidSeriesName(name) {
		return nil, fmt.Errorf("%w: series name %q", core.ErrBadKey, name)
	}
	sr := s.series[name]
	if sr == nil {
		if !create {
			return nil, fmt.Errorf("%w: series %q", core.ErrNoTile, name)
		}
		sr = &Series{}
		s.series[name] = sr
	}
	return sr, nil
}

// List implements core.SeriesStore.
func (s *SeriesStore) List(ctx context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, core.ErrClosed
	}
	var out []string
	for name := range s.series {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close implements core.SeriesStore.
func (s *SeriesStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Append implements core.Series.
func (sr *Series) Append(ctx context.Context, rec core.Record) (uint64, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.closed {
		return 0, core.ErrClosed
	}
	switch rec.Kind {
	case core.RecordDelta, core.RecordKeyframe, core.RecordSnapshot:
	default:
		return 0, fmt.Errorf("series: unknown record kind %d", rec.Kind)
	}
	if n := len(sr.recs); n > 0 && rec.TEnd() < sr.recs[n-1].TEnd() {
		return 0, fmt.Errorf("%w: TEnd %d before %d", core.ErrOutOfOrder, rec.TEnd(), sr.recs[n-1].TEnd())
	}
	seq := uint64(len(sr.recs))
	rec.Seq = seq
	rec.Len = len(rec.Payload)
	rec.Payload = append([]byte(nil), rec.Payload...)
	sr.recs = append(sr.recs, rec)
	return seq, nil
}

// Len implements core.Series.
func (sr *Series) Len() uint64 {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return uint64(len(sr.recs))
}

// Meta implements core.Series.
func (sr *Series) Meta(seq uint64) (core.RecordMeta, error) {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	if seq >= uint64(len(sr.recs)) {
		return core.RecordMeta{}, fmt.Errorf("%w: record %d", core.ErrNoVersion, seq)
	}
	return sr.recs[seq].RecordMeta, nil
}

// Read implements core.Series.
func (sr *Series) Read(ctx context.Context, seq uint64) (core.Record, error) {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	if sr.closed {
		return core.Record{}, core.ErrClosed
	}
	if seq >= uint64(len(sr.recs)) {
		return core.Record{}, fmt.Errorf("%w: record %d", core.ErrNoVersion, seq)
	}
	r := sr.recs[seq]
	r.Payload = append([]byte(nil), r.Payload...)
	return r, nil
}

// Chain implements core.Series.
func (sr *Series) Chain(ctx context.Context, t core.TimeMs) ([]core.Record, error) {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	if sr.closed {
		return nil, core.ErrClosed
	}
	end := sort.Search(len(sr.recs), func(i int) bool { return sr.recs[i].TEnd() > t })
	if end == 0 {
		return nil, nil
	}
	start := 0
	for i := end - 1; i >= 0; i-- {
		if sr.recs[i].Kind.IsBase() {
			start = i
			break
		}
	}
	out := make([]core.Record, 0, end-start)
	for _, r := range sr.recs[start:end] {
		r.Payload = append([]byte(nil), r.Payload...)
		out = append(out, r)
	}
	return out, nil
}

// Sync implements core.Series.
func (sr *Series) Sync() error { return nil }

// Close implements core.Series. The data stays in the store; reopening returns it.
func (sr *Series) Close() error { return nil }
