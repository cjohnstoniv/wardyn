// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"container/list"
	"sync"
	"time"
)

// Cache bounds: a key is held at most cacheTTL after it was unwrapped, and at
// most cacheMax keys at once. The cache saves the key service's round trip, not
// the revocation check (Manager.Key reads destroyed_at on every use).
const (
	cacheTTL = 5 * time.Minute
	cacheMax = 1024
)

// keyID names one generation of a subject's key.
type keyID struct {
	owner, purpose string
	version        int
}

type cacheEntry struct {
	id      keyID
	key     []byte
	expires time.Time
}

// cache is a bounded LRU of unwrapped keys with a fixed lifetime. Every key it
// drops is overwritten first, which is best effort only: Go's runtime may
// already have copied the slice.
type cache struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	now func() time.Time
	ll  *list.List // front is the most recently used
	m   map[keyID]*list.Element
}

func newCache(max int, ttl time.Duration) *cache {
	return &cache{max: max, ttl: ttl, now: time.Now, ll: list.New(), m: map[keyID]*list.Element{}}
}

// get returns a copy of id's key, which the caller owns and clears.
func (c *cache) get(id keyID) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[id]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expires) {
		c.drop(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return append([]byte(nil), e.key...), true
}

// put stores a copy of key; the caller keeps, and clears, its own.
func (c *cache) put(id keyID, key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[id]; ok {
		c.drop(el)
	}
	c.m[id] = c.ll.PushFront(&cacheEntry{id: id, key: append([]byte(nil), key...), expires: c.now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		c.drop(c.ll.Back())
	}
}

func (c *cache) evict(id keyID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[id]; ok {
		c.drop(el)
	}
}

// evictSubject drops every generation of (owner, purpose).
func (c *cache) evictSubject(owner, purpose string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, el := range c.m {
		if id.owner == owner && id.purpose == purpose {
			c.drop(el)
		}
	}
}

func (c *cache) drop(el *list.Element) {
	e := c.ll.Remove(el).(*cacheEntry)
	clear(e.key)
	delete(c.m, e.id)
}
