// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"sync"
	"time"
)

// sshKeyLimiterCap is the most registered keys sshKeyLimiters tracks at once.
const sshKeyLimiterCap = 1024

// sshKeyLimiters rate-bounds the ssh.authenticate refusal rows that name a registered key, one
// token bucket per key fingerprint with the shape of authFailedLimiter (authFailedBurst,
// authFailedRatePerSec). Such a row is written in the PublicKeyCallback, before any signature is
// verified, so anyone who knows a public key can cause it: the bound has to hold per key, and a
// flood of unattributed failures must not spend it (the shared limiter is theirs). The zero value is
// ready to use.
type sshKeyLimiters struct {
	mu      sync.Mutex
	limit   int // sshKeyLimiterCap when zero
	buckets map[string]*authFailedLimiter
}

// allow charges one refusal to the key's bucket. tracked is false when the key has no bucket and none
// can be made, because the map is full of keys that are not idle; the caller then charges the shared
// limiter instead. A bucket that has refilled to its burst holds nothing a fresh one lacks, so the
// oldest such idle one is evicted to admit a new key.
func (k *sshKeyLimiters) allow(fingerprint string, now time.Time) (admitted, tracked bool) {
	k.mu.Lock()
	b, ok := k.buckets[fingerprint]
	if !ok {
		limit := k.limit
		if limit == 0 {
			limit = sshKeyLimiterCap
		}
		if k.buckets == nil {
			k.buckets = map[string]*authFailedLimiter{}
		}
		if len(k.buckets) >= limit && !k.evictIdle(now) {
			k.mu.Unlock()
			return false, false
		}
		b = &authFailedLimiter{}
		k.buckets[fingerprint] = b
	}
	// Charged under k.mu, so evictIdle never meets a bucket between its lookup and its charge: a holder
	// charging after the unlock could have its bucket evicted and charge an orphan, one extra burst.
	admitted = b.allow(now)
	k.mu.Unlock()
	return admitted, true
}

// evictIdle removes the key whose bucket has been idle longest, if any has refilled to its burst.
// The caller holds k.mu.
func (k *sshKeyLimiters) evictIdle(now time.Time) bool {
	idle := time.Duration(authFailedBurst/authFailedRatePerSec) * time.Second
	oldest, oldestAt := "", now
	for fp, b := range k.buckets {
		b.mu.Lock()
		last := b.last
		b.mu.Unlock()
		// A zero last is a bucket made a moment ago and not yet charged: never idle.
		if !last.IsZero() && now.Sub(last) >= idle && (oldest == "" || last.Before(oldestAt)) {
			oldest, oldestAt = fp, last
		}
	}
	if oldest == "" {
		return false
	}
	delete(k.buckets, oldest)
	return true
}
