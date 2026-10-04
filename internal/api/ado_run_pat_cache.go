// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Each `minted_pat` run's current personal access token. The ado_run_pats row
// records that a token exists (so it can be revoked after a crash); the value
// lives in ado_run_pat_state (internal/adorunpat), sealed under the run owner's
// key, so a resolve served by any replica hands out the token another created,
// and a restart loses nothing. A server with no such store (a test, a build with
// no database) keeps it in this process, and a restart loses it: the run's next
// resolve creates another.
//
// Every mint and revoke for one run takes the run's token lock (db.ADORunTokenLockClass),
// a Postgres advisory lock, so a dozen hosts resolving at once, on one replica or
// several, create one token, and a pause's revoke cannot interleave with a mint.
// A lock that cannot be taken refuses the work; nothing proceeds unlocked.
//
// The proxy keeps one header per Azure DevOps host and nothing here can reach
// it, so a token a host may still hold is never revoked early: renewal and
// widening put a new token in the cache and leave the old one to its own
// validTo. The proxy asks again with stale_jti when Azure DevOps refuses the
// header it holds, and gets the cache's token.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/db"
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

// adoRunPATCache is the in-process token map, the state of a server with no ADORunPATs store.
type adoRunPATCache struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*adoRunPATEntry
	// sweptAt is when sweepRunPATs last ran; zero runs the first tick.
	sweptAt time.Time
}

// adoRunPATEntry is one run's tokens, valid while the caller holds the run's token lock.
type adoRunPATEntry struct {
	// owner is the run owner's secret subject, the key the value is sealed under; "" until a
	// token has been created or a record read.
	owner string
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
}

// lockRunPAT takes runID's token lock and returns the run's state, read after the lock is held.
// The returned context is the guarded work's, cancelled if the lock is lost; call unlock when
// done, then save or drop what changed before it.
func (s *Server) lockRunPAT(ctx context.Context, runID uuid.UUID) (context.Context, *adoRunPATEntry, func(), error) {
	lctx, unlock, err := s.lock(ctx, db.ADORunTokenLockClass, runID.String())
	if err != nil {
		return ctx, nil, nil, err
	}
	e, err := s.loadRunPAT(lctx, runID)
	if err != nil {
		unlock()
		return ctx, nil, nil, err
	}
	return lctx, e, unlock, nil
}

func (s *Server) loadRunPAT(ctx context.Context, runID uuid.UUID) (*adoRunPATEntry, error) {
	c := &s.adoRunPATs
	if s.cfg.ADORunPATs == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.runs == nil {
			c.runs = map[uuid.UUID]*adoRunPATEntry{}
		}
		e := c.runs[runID]
		if e == nil {
			e = &adoRunPATEntry{}
			c.runs[runID] = e
		}
		return e, nil
	}
	rec, found, err := s.cfg.ADORunPATs.Load(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errADOPATStateUnavailable, err)
	}
	e := &adoRunPATEntry{}
	if found {
		e.owner, e.paused, e.forcedAt = rec.Owner, rec.Paused, rec.ForcedAt
		e.cur = adoPAT{AuthorizationID: rec.AuthorizationID, Token: rec.Token, Scope: rec.Scope, ValidTo: rec.ValidTo}
		for _, c := range rec.Capabilities {
			e.caps = append(e.caps, adoscope.Capability(c))
		}
	}
	return e, nil
}

// saveRunPAT writes e as runID's state, under the lock loadRunPAT's caller holds. An entry with
// nothing in it is deleted instead.
func (s *Server) saveRunPAT(ctx context.Context, runID uuid.UUID, e *adoRunPATEntry) error {
	if s.cfg.ADORunPATs == nil {
		return nil
	}
	if e.owner == "" || (e.cur.Token == "" && !e.paused && len(e.caps) == 0 && e.forcedAt.IsZero()) {
		return s.cfg.ADORunPATs.Delete(ctx, runID)
	}
	rec := adorunpat.Record{
		Owner: e.owner, AuthorizationID: e.cur.AuthorizationID, Token: e.cur.Token, Scope: e.cur.Scope,
		ValidTo: e.cur.ValidTo, Paused: e.paused, ForcedAt: e.forcedAt,
	}
	for _, c := range e.caps {
		rec.Capabilities = append(rec.Capabilities, string(c))
	}
	if err := s.cfg.ADORunPATs.Save(ctx, runID, e.owner, rec); err != nil {
		return fmt.Errorf("%w: %w", errADOPATStateUnavailable, err)
	}
	return nil
}

// dropRunPAT forgets runID's state: the run ended, or its tokens were revoked.
func (s *Server) dropRunPAT(ctx context.Context, runID uuid.UUID) error {
	if s.cfg.ADORunPATs != nil {
		return s.cfg.ADORunPATs.Delete(ctx, runID)
	}
	c := &s.adoRunPATs
	c.mu.Lock()
	delete(c.runs, runID)
	c.mu.Unlock()
	return nil
}

// covers reports whether the entry's current token was built from every
// capability in want.
func (e *adoRunPATEntry) covers(want []adoscope.Capability) bool {
	return e.cur.Token != "" && subsetOf(want, e.caps)
}
