// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"container/list"
	"slices"
	"sync"
	"time"
)

// How long a verdict is kept, and how many. An admission is kept an hour, so a busy image is checked about
// once an hour per replica. A refusal is kept a minute, so a signature pushed after a refusal counts soon,
// while a retry storm still cannot hammer the registry. A registry that could not be read is never kept.
const (
	PositiveTTL  = time.Hour
	NegativeTTL  = time.Minute
	CacheEntries = 1024
)

// cacheKey is the requirements' fingerprint (signers, trust roots, profile set) and the image digest, so any
// change to the policy or a root misses.
type cacheKey struct {
	fingerprint [32]byte
	digest      string
}

type cacheEntry struct {
	key     cacheKey
	verdict Verdict
	expires time.Time
}

// verdictCache is a least-recently-used map of verdicts with per-entry expiry.
type verdictCache struct {
	mu    sync.Mutex
	order *list.List // front = most recently used
	byKey map[cacheKey]*list.Element
}

func newVerdictCache() *verdictCache {
	return &verdictCache{order: list.New(), byKey: map[cacheKey]*list.Element{}}
}

func (c *verdictCache) get(k cacheKey, now time.Time) (Verdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[k]
	if !ok {
		return Verdict{}, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.expires) {
		c.order.Remove(el)
		delete(c.byKey, k)
		return Verdict{}, false
	}
	c.order.MoveToFront(el)
	return cloneVerdict(e.verdict), true
}

// put keeps v when it is a verdict worth keeping: an admission or a refusal about the signatures.
func (c *verdictCache) put(k cacheKey, v Verdict, now time.Time) {
	var ttl time.Duration
	switch v.Reason {
	case "":
		ttl = PositiveTTL
	case SignatureMissing, SignatureInvalid, AttestationMissing:
		ttl = NegativeTTL
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[k]; ok {
		el.Value = &cacheEntry{key: k, verdict: cloneVerdict(v), expires: now.Add(ttl)}
		c.order.MoveToFront(el)
		return
	}
	c.byKey[k] = c.order.PushFront(&cacheEntry{key: k, verdict: cloneVerdict(v), expires: now.Add(ttl)})
	for c.order.Len() > CacheEntries {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.byKey, last.Value.(*cacheEntry).key)
	}
}

// cloneVerdict copies the slices, so a caller changing its verdict cannot change the kept one.
func cloneVerdict(v Verdict) Verdict {
	v.Signers = slices.Clone(v.Signers)
	v.Attestations = slices.Clone(v.Attestations)
	return v
}
