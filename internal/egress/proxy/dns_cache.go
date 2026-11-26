// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// dnsCacheTTL is how long one lookup's answer is reused. The guard vets the answer on every use, as it vets
// a fresh one, so the cache saves only the lookup: it never admits an address the guard would refuse.
const dnsCacheTTL = 30 * time.Second

// dnsCacheMax bounds the hosts held; past it the cache starts empty again.
const dnsCacheMax = 4096

// cachedResolver answers a host from its last successful lookup for dnsCacheTTL, and shares one lookup among
// the callers that miss together. A failed or empty lookup is never kept.
type cachedResolver struct {
	next   resolver
	now    func() time.Time
	flight singleflight.Group

	mu      sync.Mutex
	answers map[string]cachedAnswer
}

type cachedAnswer struct {
	ips []net.IP
	at  time.Time
}

func newCachedResolver(next resolver, now func() time.Time) *cachedResolver {
	return &cachedResolver{next: next, now: now, answers: make(map[string]cachedAnswer)}
}

func (c *cachedResolver) LookupIP(host string) ([]net.IP, error) {
	key := strings.TrimSuffix(strings.ToLower(host), ".")
	if ips, ok := c.cached(key); ok {
		return ips, nil
	}
	v, err, _ := c.flight.Do(key, func() (any, error) {
		if ips, ok := c.cached(key); ok { // a lookup that ended while this caller was on its way in
			return ips, nil
		}
		ips, err := c.next.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return ips, err
		}
		c.mu.Lock()
		if len(c.answers) >= dnsCacheMax {
			clear(c.answers)
		}
		c.answers[key] = cachedAnswer{ips: slices.Clone(ips), at: c.now()}
		c.mu.Unlock()
		return ips, nil
	})
	ips, _ := v.([]net.IP)
	return slices.Clone(ips), err
}

// cached returns key's answer while it is younger than dnsCacheTTL.
func (c *cachedResolver) cached(key string) ([]net.IP, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.answers[key]
	if !ok || c.now().Sub(a.at) >= dnsCacheTTL {
		return nil, false
	}
	return slices.Clone(a.ips), true
}
