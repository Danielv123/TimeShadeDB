package terrain

import (
	"sync"
	"sync/atomic"

	"timeshadedb/internal/core"
)

// imageBytes is the accounted size of one cached tile image.
const imageBytes = core.TilePixelCount*2 + 128

type cacheKey struct {
	ds   *dsState
	tile core.TileCoord
	v    core.Version
}

type cacheNode struct {
	key        cacheKey
	im         *core.TileImage
	prev, next *cacheNode
}

// decodedCache is an LRU of decoded tile versions (RGB565), bounded by bytes. Cached
// images are immutable; callers clone before handing them out.
type decodedCache struct {
	max int64

	mu         sync.Mutex
	used       int64
	m          map[cacheKey]*cacheNode
	head, tail *cacheNode

	hits, misses atomic.Uint64
}

func newDecodedCache(max int64) *decodedCache {
	return &decodedCache{max: max, m: map[cacheKey]*cacheNode{}}
}

func (c *decodedCache) get(k cacheKey) *core.TileImage {
	c.mu.Lock()
	n := c.m[k]
	if n == nil {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	c.moveFront(n)
	im := n.im // read under the lock: put may replace it
	c.mu.Unlock()
	c.hits.Add(1)
	return im
}

// has reports whether k is cached without counting a hit or miss.
func (c *decodedCache) has(k cacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k] != nil
}

func (c *decodedCache) put(k cacheKey, im *core.TileImage) {
	if c.max < imageBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.m[k]; n != nil {
		// Same key, same content: keep the cached image.
		c.moveFront(n)
		return
	}
	n := &cacheNode{key: k, im: im}
	c.m[k] = n
	c.pushFront(n)
	c.used += imageBytes
	for c.used > c.max && c.tail != nil {
		old := c.tail
		c.unlink(old)
		delete(c.m, old.key)
		c.used -= imageBytes
	}
}

// dropDatastore removes every entry of ds.
func (c *decodedCache) dropDatastore(ds *dsState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, n := range c.m {
		if k.ds == ds {
			c.unlink(n)
			delete(c.m, k)
			c.used -= imageBytes
		}
	}
}

func (c *decodedCache) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

func (c *decodedCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[cacheKey]*cacheNode{}
	c.head, c.tail = nil, nil
	c.used = 0
}

func (c *decodedCache) pushFront(n *cacheNode) {
	n.prev, n.next = nil, c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
}

func (c *decodedCache) unlink(n *cacheNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (c *decodedCache) moveFront(n *cacheNode) {
	if c.head == n {
		return
	}
	c.unlink(n)
	c.pushFront(n)
}
