// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"maps"
	"sync"
)

// counters are the per-channel series behind wardyn_approval_notify_failed_total and
// wardyn_approval_notify_suppressed_total, read by the metrics scrape. Per replica, like every
// counter there. Keys are channel ids, which the config bounds.
var counters = &channelCounters{failed: map[string]int64{}, suppressed: map[string]int64{}}

type channelCounters struct {
	mu         sync.Mutex
	failed     map[string]int64
	suppressed map[string]int64
}

func (c *channelCounters) addFailed(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed[channel]++
}

func (c *channelCounters) addSuppressed(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suppressed[channel]++
}

// Counts is a snapshot of both series.
type Counts struct {
	Failed, Suppressed map[string]int64
}

// Snapshot copies the counters.
func Snapshot() Counts {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	return Counts{Failed: maps.Clone(counters.failed), Suppressed: maps.Clone(counters.suppressed)}
}

// Channels lists the configured channel ids, so a scrape can emit a zero for a channel that has
// not failed yet.
func Channels() []string {
	c := active.Load()
	if c == nil {
		return nil
	}
	ids := make([]string, len(c.Channels))
	for i, ch := range c.Channels {
		ids[i] = ch.ID
	}
	return ids
}
