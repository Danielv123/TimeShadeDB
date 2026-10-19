package tileenc

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// LRU is a sharded least-recently-used cache bounded by a byte budget. The size of each
// value is given by the size function. It is safe for concurrent use. Values are shared
// between callers and must be treated as read-only.
//
// Each shard owns an equal part of the budget, so a single value larger than
// budget/shards is not cached.
type LRU[K comparable, V any] struct {
	seed   maphash.Seed
	shards []lruShard[K, V]
	size   func(V) int64

	hits, misses, evictions atomic.Uint64
	bytes                   atomic.Int64
	entries                 atomic.Int64
}

type lruEntry[K comparable, V any] struct {
	key        K
	val        V
	size       int64
	prev, next *lruEntry[K, V]
}

type lruShard[K comparable, V any] struct {
	mu    sync.Mutex
	max   int64
	used  int64
	items map[K]*lruEntry[K, V]
	head  lruEntry[K, V] // sentinel: head.next is the most recent, head.prev the oldest
}

// LRUStats is a snapshot of the cache counters.
type LRUStats struct {
	Hits, Misses, Evictions uint64
	Bytes, Entries          int64
	MaxBytes                int64
}

// NewLRU returns a cache holding at most maxBytes (as measured by size) in the given
// number of shards (at least 1). maxBytes <= 0 disables caching.
func NewLRU[K comparable, V any](maxBytes int64, shards int, size func(V) int64) *LRU[K, V] {
	if shards < 1 {
		shards = 1
	}
	c := &LRU[K, V]{seed: maphash.MakeSeed(), shards: make([]lruShard[K, V], shards), size: size}
	for i := range c.shards {
		s := &c.shards[i]
		s.max = maxBytes / int64(shards)
		s.items = map[K]*lruEntry[K, V]{}
		s.head.next = &s.head
		s.head.prev = &s.head
	}
	return c
}

func (c *LRU[K, V]) shard(k K) *lruShard[K, V] {
	if len(c.shards) == 1 {
		return &c.shards[0]
	}
	return &c.shards[maphash.Comparable(c.seed, k)%uint64(len(c.shards))]
}

func (s *lruShard[K, V]) unlink(e *lruEntry[K, V]) {
	e.prev.next = e.next
	e.next.prev = e.prev
}

func (s *lruShard[K, V]) pushFront(e *lruEntry[K, V]) {
	e.prev = &s.head
	e.next = s.head.next
	s.head.next.prev = e
	s.head.next = e
}

// Get returns the cached value and marks it recently used.
func (c *LRU[K, V]) Get(k K) (V, bool) {
	s := c.shard(k)
	s.mu.Lock()
	e := s.items[k]
	if e == nil {
		s.mu.Unlock()
		c.misses.Add(1)
		var zero V
		return zero, false
	}
	s.unlink(e)
	s.pushFront(e)
	v := e.val
	s.mu.Unlock()
	c.hits.Add(1)
	return v, true
}

// Put stores a value, replacing any previous value for k, and evicts the least recently
// used entries of the shard until it fits its budget.
func (c *LRU[K, V]) Put(k K, v V) {
	sz := c.size(v)
	s := c.shard(k)
	if sz > s.max {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.items[k]; old != nil {
		s.unlink(old)
		s.used -= old.size
		c.bytes.Add(-old.size)
		c.entries.Add(-1)
		delete(s.items, k)
	}
	for s.used+sz > s.max && s.head.prev != &s.head {
		old := s.head.prev
		s.unlink(old)
		delete(s.items, old.key)
		s.used -= old.size
		c.bytes.Add(-old.size)
		c.entries.Add(-1)
		c.evictions.Add(1)
	}
	e := &lruEntry[K, V]{key: k, val: v, size: sz}
	s.items[k] = e
	s.pushFront(e)
	s.used += sz
	c.bytes.Add(sz)
	c.entries.Add(1)
}

// Delete removes k.
func (c *LRU[K, V]) Delete(k K) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.items[k]; e != nil {
		s.unlink(e)
		delete(s.items, k)
		s.used -= e.size
		c.bytes.Add(-e.size)
		c.entries.Add(-1)
	}
}

// Purge removes every entry.
func (c *LRU[K, V]) Purge() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		c.bytes.Add(-s.used)
		c.entries.Add(-int64(len(s.items)))
		s.items = map[K]*lruEntry[K, V]{}
		s.head.next = &s.head
		s.head.prev = &s.head
		s.used = 0
		s.mu.Unlock()
	}
}

// Stats returns the counters.
func (c *LRU[K, V]) Stats() LRUStats {
	var max int64
	for i := range c.shards {
		max += c.shards[i].max
	}
	return LRUStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Bytes:     c.bytes.Load(),
		Entries:   c.entries.Load(),
		MaxBytes:  max,
	}
}
