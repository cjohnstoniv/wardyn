// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// healthzProbe is the part of /healthz that costs a read: the runner's capabilities, the connected Kubernetes
// runners' NetworkPolicy verdicts and the eBPF sensor's status. It holds no caller's identity, and nothing writes
// to it once built, so every request may share one.
type healthzProbe struct {
	runnerName string
	caps       []types.ConfinementClass
	substrates map[types.ConfinementClass]string
	own        runner.NetPolCheck
	ownUnread  bool // a Kubernetes daemon whose own verdict could not be read
	runners    runner.NetPolCheck
	gtStatus   map[string]any
}

// healthzProbeCache holds the last probe and when its read started.
type healthzProbeCache struct {
	mu    sync.Mutex
	at    time.Time
	ok    bool
	p     healthzProbe
	reads atomic.Uint64 // probes kept so far
}

// healthzProbeFor returns a probe whose read started less than cfg.HealthzProbeTTL ago, reading a new one when there
// is none. mu is held across the read, so one read runs at a time and a burst of polls costs one. Nothing is served
// past the TTL except to a caller that waited for the read itself: when the store hangs, the next read hangs too and the probe fails as it would with no cache. A read
// whose request ended is not kept, since what it returned may be partial. A zero TTL reads every time.
func (s *Server) healthzProbeFor(ctx context.Context) healthzProbe {
	ttl := s.cfg.HealthzProbeTTL
	if ttl <= 0 {
		return s.readHealthzProbe(ctx)
	}
	c := &s.healthzCache
	seen := c.reads.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.cfg.Now()
	// A probe whose read started less than ttl ago, or one kept while this caller waited for mu: that read ended
	// after this request began, so its answer is at most one read's length old. Without the second arm a read
	// slower than the TTL has expired before its waiters see it, and every queued request reads in turn.
	if c.ok && (c.reads.Load() != seen || (!now.Before(c.at) && now.Sub(c.at) < ttl)) {
		return c.p
	}
	p := s.readHealthzProbe(ctx)
	if ctx.Err() == nil {
		c.at, c.p, c.ok = now, p, true
		c.reads.Add(1)
	}
	return p
}

// readHealthzProbe makes the probe's reads, in handleHealthz's order before the cache.
func (s *Server) readHealthzProbe(ctx context.Context) healthzProbe {
	var p healthzProbe
	if s.cfg.Runner != nil {
		p.runnerName = s.cfg.Runner.Name()
		if c, err := s.cfg.Runner.Capabilities(ctx); err == nil {
			p.caps = c.ConfinementClasses
			p.substrates = c.Resolved
			p.own.Verdict = k8sNetpolVerdict(p.runnerName, c)
			if runner.KnownNetPolVerdict(c.NetworkPolicyCheck.Verdict) {
				p.own.At = c.NetworkPolicyCheck.At
			}
		} else {
			p.ownUnread = p.runnerName == "k8s"
		}
	}
	p.runners = s.runnersNetPolCheck(ctx)
	p.gtStatus = s.ebpfGroundtruthStatus(ctx)
	return p
}
