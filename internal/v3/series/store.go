package series

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"timeshadedb/internal/v3/core"
)

// Options configure a Store.
type Options struct {
	// MaxOpen is the number of idle series kept open (files and index in memory).
	// Series in use by a handle are never closed. Default 256.
	MaxOpen int
	// MaxIdleIndexBytes bounds the memory of the indexes of idle open series
	// (40 bytes per record). Default 32 MiB. Series in use are not counted.
	MaxIdleIndexBytes int64
	// NoSync skips fsync (tests and bulk imports that can be redone). Records are still
	// written in the same order.
	NoSync bool
	// OnRecover, if set, is called when opening a series repaired something.
	OnRecover func(name string, rep RecoveryReport)
}

// Store is a directory of series. It implements core.SeriesStore. Series names are
// slash-separated relative paths; series "a/b" lives in <root>/a/b.blk and .idx.
type Store struct {
	root string
	opt  Options

	mu     sync.Mutex
	closed bool
	files  map[string]*file
	idle   lruList // files with refs == 0, most recently used first
	// idleBytes is the index memory of the idle files.
	idleBytes int64

	opens, evictions atomic.Uint64
}

var _ core.SeriesStore = (*Store)(nil)

// Open returns a store rooted at dir, creating the directory if needed.
func Open(dir string, opt Options) (*Store, error) {
	if opt.MaxOpen <= 0 {
		opt.MaxOpen = 256
	}
	if opt.MaxIdleIndexBytes <= 0 {
		opt.MaxIdleIndexBytes = 32 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: dir, opt: opt, files: map[string]*file{}}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

// ValidName reports whether name is a slash-separated path of id-safe parts
// ([A-Za-z0-9_~.-]) with no empty, "." or ".." part. It is the rule of
// memstore.ValidSeriesName.
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for _, p := range strings.Split(name, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
		for i := 0; i < len(p); i++ {
			c := p[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '~' || c == '.' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Open implements core.SeriesStore.
func (s *Store) Open(ctx context.Context, name string, create bool) (core.Series, error) {
	h, err := s.OpenSeries(name, create)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// OpenSeries is Open returning the concrete handle, which has extra read helpers.
func (s *Store) OpenSeries(name string, create bool) (*Series, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: series name %q", core.ErrBadKey, name)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, core.ErrClosed
	}
	if f := s.files[name]; f != nil {
		f.refs++
		if f.lru != nil {
			s.idleBytes -= f.lru.bytes
			s.idle.remove(f.lru)
			f.lru = nil
		}
		s.mu.Unlock()
		<-f.ready
		if f.err != nil {
			s.release(f)
			if errors.Is(f.err, core.ErrNoTile) && create {
				// Another opener raced us without create; retry with create.
				return s.OpenSeries(name, create)
			}
			return nil, f.err
		}
		return &Series{f: f, s: s}, nil
	}
	f := &file{
		name:   name,
		path:   filepath.Join(s.root, filepath.FromSlash(name)),
		noSync: s.opt.NoSync,
		refs:   1,
		ready:  make(chan struct{}),
	}
	s.files[name] = f
	s.mu.Unlock()

	rep, err := f.load(create)
	s.opens.Add(1)
	if err == nil && rep.any() && s.opt.OnRecover != nil {
		s.opt.OnRecover(name, rep)
	}
	s.mu.Lock()
	f.err = err
	if err != nil {
		delete(s.files, name)
	}
	close(f.ready)
	s.mu.Unlock()
	if err != nil {
		s.release(f)
		return nil, err
	}
	return &Series{f: f, s: s}, nil
}

// release drops one reference and closes idle files beyond MaxOpen.
func (s *Store) release(f *file) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.refs--
	if f.refs > 0 || f.err != nil || s.files[f.name] != f {
		return
	}
	f.lru = s.idle.pushFront(f)
	f.mu.RLock()
	f.lru.bytes = int64(cap(f.entries)) * entrySize
	f.mu.RUnlock()
	s.idleBytes += f.lru.bytes
	for s.idle.len > s.opt.MaxOpen || (s.idleBytes > s.opt.MaxIdleIndexBytes && s.idle.len > 0) {
		old := s.idle.back()
		s.idleBytes -= old.bytes
		s.idle.remove(old.f.lru)
		old.f.lru = nil
		delete(s.files, old.f.name)
		_ = old.f.close() // records are already durable or are rebuilt on open
		s.evictions.Add(1)
	}
}

// List implements core.SeriesStore.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, core.ErrClosed
	}
	dir := ""
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		dir = prefix[:i]
	}
	start := filepath.Join(s.root, filepath.FromSlash(dir))
	var out []string
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".blk") {
			return nil
		}
		rel, err := filepath.Rel(s.root, strings.TrimSuffix(p, ".blk"))
		if err != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if strings.HasPrefix(name, prefix) && ValidName(name) {
			out = append(out, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// SyncAll fsyncs every open series with unsynced records.
func (s *Store) SyncAll() error {
	s.mu.Lock()
	files := make([]*file, 0, len(s.files))
	for _, f := range s.files {
		files = append(files, f)
	}
	s.mu.Unlock()
	var first error
	for _, f := range files {
		select {
		case <-f.ready:
		default:
			continue
		}
		if f.err != nil {
			continue
		}
		if err := f.sync(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Close implements core.SeriesStore. It flushes and closes every series, including
// ones with open handles (whose later calls return core.ErrClosed).
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	files := s.files
	s.files = map[string]*file{}
	s.idle = lruList{}
	s.idleBytes = 0
	s.mu.Unlock()
	var first error
	for _, f := range files {
		<-f.ready
		if f.err != nil {
			continue
		}
		if err := f.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Stats describe the store's open series.
type Stats struct {
	Open           int    // series open (in use or idle)
	Idle           int    // idle series kept open
	IndexBytes     int64  // memory held by the open indexes
	IdleIndexBytes int64  // part of IndexBytes held by idle series
	Opens          uint64 // series loads from disk
	Evictions      uint64 // idle series closed to stay within MaxOpen
}

// Stats returns current counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Open: len(s.files), Idle: s.idle.len, IdleIndexBytes: s.idleBytes, Opens: s.opens.Load(), Evictions: s.evictions.Load()}
	for _, f := range s.files {
		select {
		case <-f.ready:
			f.mu.RLock()
			st.IndexBytes += int64(cap(f.entries)) * entrySize
			f.mu.RUnlock()
		default:
		}
	}
	return st
}

// entrySize is the in-memory size of one index entry.
const entrySize = 40

// lruList is a small intrusive doubly linked list.
type lruNode struct {
	f          *file
	bytes      int64 // index memory when the file went idle
	prev, next *lruNode
}

type lruList struct {
	head, tail *lruNode
	len        int
}

func (l *lruList) pushFront(f *file) *lruNode {
	n := &lruNode{f: f, next: l.head}
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
	l.len++
	return n
}

func (l *lruList) remove(n *lruNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
	l.len--
}

func (l *lruList) back() *lruNode { return l.tail }
