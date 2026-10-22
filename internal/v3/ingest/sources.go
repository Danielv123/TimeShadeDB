package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
)

// Sources is the ingest state kept per forwarder source (envelope "src";
// docs/v3/API.md 5 "Horizon", FORMAT.md 5.8):
//
//   - per map the source fed, W: the newest message time applied from it, or the newest
//     receive time of a caught_up envelope (the forwarder has nothing older pending).
//     While the source is held, the commit loop closes no window of that map past W minus
//     its grace (Holds), so data the source still sends in time order is never late: a
//     backlog after a downtime lands where it belongs. A live source's W is a few seconds
//     behind the clock, so it holds nothing back beyond that.
//   - seen: when the server last heard from it (wall clock). A source is held until Hold
//     after that. Every source loaded from disk counts as seen at start, so after a
//     server restart the horizon waits up to Hold for each source's backlog; a source
//     that stopped for good releases its maps after Hold.
//   - the last applied envelope's (seq, recv_ms): envelopes of a retried body up to it
//     are skipped as duplicates, so a retry neither runs the journey rules again nor
//     re-anchors the clock (Duplicate).
//
// The table is saved to <db>/sources.json by Save (the commit loop calls it every step).
// It is safe for concurrent use.
type Sources struct {
	path string
	hold time.Duration
	wall func() time.Time

	mu    sync.Mutex
	m     map[string]*source
	dirty bool
}

type source struct {
	Src    string                 `json:"src"`
	Seq    *int64                 `json:"seq,omitempty"`
	Recv   core.TimeMs            `json:"recv_ms,omitempty"`
	SeenMs int64                  `json:"seen_ms"`
	Maps   map[string]core.TimeMs `json:"maps"`
}

type sourcesFile struct {
	Version int       `json:"version"`
	Sources []*source `json:"sources"`
}

// sourceForget is how long a source that is not heard from stays in the table.
const sourceForget = 7 * 24 * time.Hour

// OpenSources loads <dir>/sources.json (when dir is not empty) and returns the table.
// hold is how long a source holds its maps after it was last heard from (0: sources
// hold nothing, the horizon follows the clock alone). wall is the wall clock (nil:
// time.Now). An unreadable file is logged by the caller through the error and ignored.
func OpenSources(dir string, hold time.Duration, wall func() time.Time) (*Sources, error) {
	if wall == nil {
		wall = time.Now
	}
	s := &Sources{hold: hold, wall: wall, m: map[string]*source{}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, "sources.json")
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var f sourcesFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		return s, fmt.Errorf("ingest: %s is not readable, starting without sources: %v", s.path, err)
	}
	now := wall()
	for _, src := range f.Sources {
		if src == nil || src.Src == "" || now.Sub(time.UnixMilli(src.SeenMs)) > sourceForget {
			continue
		}
		if src.Maps == nil {
			src.Maps = map[string]core.TimeMs{}
		}
		// Every source counts as heard from at start: it may have a backlog to send.
		src.SeenMs = now.UnixMilli()
		s.m[src.Src] = src
	}
	return s, nil
}

// Hold returns how long a source holds its maps after it was last heard from.
func (s *Sources) Hold() time.Duration { return s.hold }

func (s *Sources) get(src string) *source {
	e := s.m[src]
	if e == nil {
		e = &source{Src: src, Maps: map[string]core.TimeMs{}}
		s.m[src] = e
	}
	return e
}

// Seen records an envelope of src for mapKey: the source is heard from now. A map the
// source has not fed before starts held at t (the envelope's own time), so the commit
// loop cannot close its windows past the source's first data before that data is
// applied.
func (s *Sources) Seen(src, mapKey string, t core.TimeMs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.get(src)
	e.SeenMs = s.wall().UnixMilli()
	if _, ok := e.Maps[mapKey]; !ok {
		e.Maps[mapKey] = t
	}
	s.dirty = true
}

// Duplicate reports whether an envelope of src with seq and recv was applied already: a
// seq at or below the last applied one, received no later than it. (A forwarder that
// restarts numbers on from a higher seq; an envelope received later than the last applied
// one cannot be a retry, whatever its seq.)
func (s *Sources) Duplicate(src string, seq int64, recv core.TimeMs) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[src]
	return e != nil && e.Seq != nil && seq <= *e.Seq && recv <= e.Recv
}

// srcMap keys per-map source state in a batch.
type srcMap struct{ src, mapKey string }

// seqMark is the last envelope of a source applied in a batch.
type seqMark struct {
	seq  int64
	recv core.TimeMs
}

// Applied records a batch the stores took: per (source, map) the newest message time
// (or caught_up receive time) it held, and per source its last envelope.
func (s *Sources) Applied(w map[srcMap]core.TimeMs, marks map[string]seqMark) {
	if len(w) == 0 && len(marks) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range w {
		e := s.get(k.src)
		if cur, ok := e.Maps[k.mapKey]; !ok || t > cur {
			e.Maps[k.mapKey] = t
		}
	}
	for src, mk := range marks {
		e := s.get(src)
		if e.Seq == nil || mk.seq > *e.Seq || mk.recv > e.Recv {
			v := mk.seq
			e.Seq, e.Recv = &v, mk.recv
		}
	}
	s.dirty = true
}

// Holds returns, per map key, the lowest W of the sources holding it: the sources heard
// from within Hold. The commit loop closes no window of the map past it minus its grace.
// Maps no source holds are absent. With Hold 0 it is empty.
func (s *Sources) Holds() map[string]core.TimeMs {
	if s.hold <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.wall().UnixMilli()
	out := map[string]core.TimeMs{}
	for _, e := range s.m {
		if now-e.SeenMs > s.hold.Milliseconds() {
			continue
		}
		for mk, t := range e.Maps {
			if cur, ok := out[mk]; !ok || t < cur {
				out[mk] = t
			}
		}
	}
	return out
}

// HeldBy returns, per map, the sources holding it with their W (for the log and tests).
func (s *Sources) HeldBy() map[string]map[string]core.TimeMs {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.wall().UnixMilli()
	out := map[string]map[string]core.TimeMs{}
	for _, e := range s.m {
		if s.hold <= 0 || now-e.SeenMs > s.hold.Milliseconds() {
			continue
		}
		for mk, t := range e.Maps {
			if out[mk] == nil {
				out[mk] = map[string]core.TimeMs{}
			}
			out[mk][e.Src] = t
		}
	}
	return out
}

// Save writes sources.json when something changed (tmp + rename), dropping sources not
// heard from for a week.
func (s *Sources) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	now := s.wall()
	f := sourcesFile{Version: 1}
	for k, e := range s.m {
		if now.Sub(time.UnixMilli(e.SeenMs)) > sourceForget {
			delete(s.m, k)
			continue
		}
		c := *e
		c.Maps = make(map[string]core.TimeMs, len(e.Maps))
		for mk, t := range e.Maps {
			c.Maps[mk] = t
		}
		f.Sources = append(f.Sources, &c)
	}
	s.dirty = false
	s.mu.Unlock()
	sort.Slice(f.Sources, func(i, j int) bool { return f.Sources[i].Src < f.Sources[j].Src })
	b, err := json.Marshal(f)
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}
