package store

import (
	"container/list"
	"sync"

	"github.com/lodastack/log"
	"github.com/lodastack/registry/model"
)

// EvictCallback is used to get a callback when a cache entry is evicted
type EvictCallback func(bucket []byte, key []byte, value []byte)

// Cache implements a non-thread safe fixed size LRU cache
type Cache struct {
	mu        sync.RWMutex
	count     int
	evictList *list.List
	items     map[string]map[string]*list.Element
	onEvict   EvictCallback

	size    uint64
	maxSize uint64

	logger *log.Logger
}

// entry is used to hold a value in the evictList
type entry struct {
	bucket []byte
	key    []byte
	value  []byte
}

func (e *entry) Size() int {
	return len(e.bucket) + len(e.key) + len(e.value)
}

// New constructs an LRU cache of the given size
func NewCache(maxSize uint64, onEvict EvictCallback) *Cache {
	// user config need check maxSize
	// if maxSize <= 0 {
	// 	return nil, errors.New("Must provide a positive size")
	// }
	c := &Cache{
		count:     0,
		maxSize:   maxSize,
		items:     make(map[string]map[string]*list.Element),
		evictList: list.New(),
		onEvict:   onEvict,
		logger:    log.New("INFO", "cache", model.LogBackend),
	}
	return c
}

// Purge is used to completely clear the cache
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for bucket, keys := range c.items {
		for k, v := range keys {
			if c.onEvict != nil {
				c.onEvict([]byte(bucket), []byte(k), v.Value.(*entry).value)
			}
			delete(c.items, string(bucket))
		}
	}
	c.count = 0
	c.size = 0
	c.evictList.Init()
}

// Add adds a value to the cache.  Returns true if an eviction occurred.
func (c *Cache) Add(bucketName []byte, key []byte, value []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check for existing bucket
	var bucket map[string]*list.Element
	var ok bool

	bucketKey := string(bucketName)

	if bucket, ok = c.items[bucketKey]; !ok {
		bucket = make(map[string]*list.Element)
		c.logger.Printf("cache new bucket %s", bucketKey)
	}

	// Check for existing item
	if ent, ok := bucket[string(key)]; ok {
		c.evictList.MoveToFront(ent)
		ent.Value.(*entry).value = value
		return false
	}

	// Add new item
	ent := &entry{bucketName, key, value}
	entry := c.evictList.PushFront(ent)
	bucket[string(key)] = entry

	c.items[bucketKey] = bucket
	c.size += uint64(ent.Size())
	c.count++

	// Verify size not exceeded
	evict := c.maxSize > 0 && c.size > c.maxSize
	if evict {
		c.removeOldest()
	}
	return evict
}

// Get looks up a key's value from the cache.
func (c *Cache) Get(bucket, key []byte) (value []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if b, ok := c.items[string(bucket)]; ok {
		if ent, ok := b[string(key)]; ok {
			c.evictList.MoveToFront(ent)
			c.logger.Debugf("Hit cache, key: %s", string(key))
			return ent.Value.(*entry).value, true
		}
	}
	return
}

// Check if a key is in the cache, without updating the recent-ness
// or deleting it for being stale.
func (c *Cache) Contains(bucket, key []byte) (ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if b, bucketok := c.items[string(bucket)]; bucketok {
		_, ok = b[string(key)]
		return ok
	}
	return
}

// Returns the key value (or undefined if not found) without updating
// the "recently used"-ness of the key.
func (c *Cache) Peek(bucket, key []byte) (value []byte, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if b, ok := c.items[string(bucket)]; ok {
		if ent, ok := b[string(key)]; ok {
			return ent.Value.(*entry).value, true
		}
	}
	return
}

// Remove Bucket removes the provided bucket from the cache, returning if the
// bucket was contained.
func (c *Cache) RemoveBucket(bucket []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if b, ok := c.items[string(bucket)]; ok {
		for _, ent := range b {
			c.removeElement(ent)
		}
		delete(c.items, string(bucket))
		return true
	}
	return false
}

// Remove removes the provided key from the cache, returning if the
// key was contained.
func (c *Cache) Remove(bucket, key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if b, ok := c.items[string(bucket)]; ok {
		if ent, ok := b[string(key)]; ok {
			c.removeElement(ent)
			return true
		}
	}
	return false
}

// RemoveOldest removes the oldest item from the cache.
func (c *Cache) RemoveOldest() ([]byte, []byte, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ent := c.evictList.Back()
	if ent != nil {
		c.removeElement(ent)
		kv := ent.Value.(*entry)
		return kv.bucket, kv.key, kv.value, true
	}
	return nil, nil, nil, false
}

// GetOldest returns the oldest entry
func (c *Cache) GetOldest() ([]byte, []byte, []byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ent := c.evictList.Back()
	if ent != nil {
		kv := ent.Value.(*entry)
		return kv.bucket, kv.key, kv.value, true
	}
	return nil, nil, nil, false
}

// Keys returns a slice of the keys in the cache, from oldest to newest.
func (c *Cache) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var keys []string
	i := 0
	for ent := c.evictList.Back(); ent != nil; ent = ent.Prev() {
		keys[i] = string(ent.Value.(*entry).bucket) + "-" + string(ent.Value.(*entry).key)
		i++
	}
	return keys
}

// Len returns the number of items in the cache.
func (c *Cache) Len() int {
	return c.evictList.Len()
}

// removeOldest removes the oldest item from the cache.
func (c *Cache) removeOldest() {
	ent := c.evictList.Back()
	if ent != nil {
		c.removeElement(ent)
	}
}

// removeElement is used to remove a given list element from the cache
func (c *Cache) removeElement(e *list.Element) {
	c.evictList.Remove(e)
	kv := e.Value.(*entry)
	if bucket, ok := c.items[string(kv.bucket)]; ok {
		delete(bucket, string(kv.key))
		c.size -= uint64(kv.Size())
		c.count--
		if c.onEvict != nil {
			c.onEvict(kv.bucket, kv.key, kv.value)
		}
	}
}