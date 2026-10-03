// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"bytes"
	"testing"
	"time"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func held(c *cache, id keyID) []byte { return c.m[id].Value.(*cacheEntry).key }

func TestCacheIsBoundedAndLeastRecentlyUsedGoesFirst(t *testing.T) {
	c := newCache(2, time.Minute)
	a, b, d := keyID{"a", "cred", 1}, keyID{"b", "cred", 1}, keyID{"d", "cred", 1}
	c.put(a, key(1))
	c.put(b, key(2))
	if _, ok := c.get(a); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	evicted := held(c, b)
	c.put(d, key(3))
	if _, ok := c.get(b); ok {
		t.Fatal("b survived a put beyond the bound")
	}
	if !bytes.Equal(evicted, make([]byte, 32)) {
		t.Fatal("an evicted key was not cleared")
	}
	for _, id := range []keyID{a, d} {
		if _, ok := c.get(id); !ok {
			t.Fatalf("%v evicted, want kept", id)
		}
	}
}

func TestCacheKeyLivesFiveMinutesAndIsClearedOnExpiry(t *testing.T) {
	if cacheTTL != 5*time.Minute {
		t.Fatalf("cacheTTL = %s, want 5m", cacheTTL)
	}
	now := time.Unix(0, 0)
	c := newCache(cacheMax, cacheTTL)
	c.now = func() time.Time { return now }
	id := keyID{"a", "cred", 1}
	c.put(id, key(7))
	kept := held(c, id)
	now = now.Add(cacheTTL - time.Second)
	if _, ok := c.get(id); !ok {
		t.Fatal("expired early")
	}
	now = now.Add(time.Second)
	if _, ok := c.get(id); ok {
		t.Fatal("still cached at the lifetime")
	}
	if !bytes.Equal(kept, make([]byte, 32)) {
		t.Fatal("an expired key was not cleared")
	}
}

// get and put hand out and keep copies: clearing a returned key never clears
// the cached one, and eviction never clears a key a caller still holds.
func TestCacheCopiesKeys(t *testing.T) {
	c := newCache(4, time.Minute)
	id := keyID{"a", "cred", 1}
	mine := key(9)
	c.put(id, mine)
	clear(mine)
	got, ok := c.get(id)
	if !ok || !bytes.Equal(got, key(9)) {
		t.Fatalf("get = (%x, %v); want the key put, unaffected by the caller clearing its own", got, ok)
	}
	clear(got)
	if again, _ := c.get(id); !bytes.Equal(again, key(9)) {
		t.Fatal("clearing a returned key cleared the cached one")
	}
	c.evictSubject("a", "cred")
	if _, ok := c.get(id); ok {
		t.Fatal("evictSubject left the key")
	}
}
