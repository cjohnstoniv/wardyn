// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Each `minted_pat` run's current personal access token, held in wardynd
// memory and nowhere else. The ado_run_pats row records that a token exists
// (so it can be revoked after a crash); only this cache holds its value, so a
// restart loses it and the run's next resolve creates another.
//
// The proxy keeps one header per Azure DevOps host and nothing here can reach
// it, so a token a host may still hold is never revoked early: renewal and
// widening put a new token in the cache and leave the old one to its own
// validTo. The proxy asks again with stale_jti when Azure DevOps refuses the
// header it holds, and gets the cache's token.

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// adoRunPATRenewWindow is how long before its validTo a run's token is
// replaced by the next resolve. It exceeds the proxy's own refresh margin
// (injectRefreshMargin, 5 min), so a host re-resolving near expiry is inside
// it and gets the new token.
const adoRunPATRenewWindow = 10 * time.Minute

// adoRunPATForcedEvery bounds the mints a stale current token can force: one
// per run per interval. A token Azure DevOps refuses for a reason a new one
// does not fix would otherwise mint on every refused request.
const adoRunPATForcedEvery = time.Minute

// adoRunPATCache is the per-run token map. Process-local, like every other
// in-memory credential bound in this package: more than one replica is
// refused by construction.
type adoRunPATCache struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*adoRunPATEntry
	// sweptAt is when sweepRunPATs last ran; zero runs the first tick.
	sweptAt time.Time
}

// adoRunPATEntry is one run's tokens. mu single-flights every mint and revoke
// for the run, so a dozen hosts resolving at once create one token.
type adoRunPATEntry struct {
	mu sync.Mutex
	// cur is the token the run's resolves hand out; zero when there is none
	// (not yet created, revoked at pause, lost to a restart).
	cur adoPAT
	// caps is what cur's scope was built from. A new token never has fewer,
	// except what the live ceiling no longer allows.
	caps []adoscope.Capability
	// paused records that pause revoked the run's tokens, so the next mint is a
	// resume rather than a restart.
	paused bool
	// forcedAt is when a stale current token last forced a mint.
	forcedAt time.Time
	// dropped marks an entry the end paths removed; a waiter holding it looks
	// the run up again rather than write into a removed entry.
	dropped bool
}

// lock returns runID's entry locked, creating it when absent. The caller must
// call the returned unlock.
func (c *adoRunPATCache) lock(runID uuid.UUID) (*adoRunPATEntry, func()) {
	for {
		c.mu.Lock()
		if c.runs == nil {
			c.runs = map[uuid.UUID]*adoRunPATEntry{}
		}
		e := c.runs[runID]
		if e == nil {
			e = &adoRunPATEntry{}
			c.runs[runID] = e
		}
		c.mu.Unlock()
		e.mu.Lock()
		if !e.dropped {
			return e, e.mu.Unlock
		}
		e.mu.Unlock()
	}
}

// drop removes runID's entry. The caller holds e locked.
func (c *adoRunPATCache) drop(runID uuid.UUID, e *adoRunPATEntry) {
	e.dropped, e.cur = true, adoPAT{}
	c.mu.Lock()
	if c.runs[runID] == e {
		delete(c.runs, runID)
	}
	c.mu.Unlock()
}

// covers reports whether the entry's current token was built from every
// capability in want.
func (e *adoRunPATEntry) covers(want []adoscope.Capability) bool {
	return e.cur.Token != "" && subsetOf(want, e.caps)
}
