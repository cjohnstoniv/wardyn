// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptEndHoldOffLimit is how long a kept run's due end may wait on its run's lock (stopKeptRun) before
// forceKeptStop stops it without the lock. It is longer than every bounded hold of that lock (a pass back
// is at most computerFolderBackTimeout), so only a hold with no bound of its own reaches it.
const keptEndHoldOffLimit = 5 * time.Minute

// keptEndHeldOff is one entry of runLeaseState.keptEndHeldOff: the end a run was due at when its lock
// first held that end off, when that was, and when the last refused pass was.
type keptEndHeldOff struct {
	until, since, last time.Time
}

// forceKeptStop is stopKeptRun's answer to a lock it could not take. Until the run's due end has been
// held off, pass after pass, for keptEndHoldOffLimit it does nothing, and the next pass retries the end.
// Past it, the sweep stops the run's sandbox without the lock and without a pass back, so an expiry
// cannot be put off by a lock held for good: it cuts egress first (the proxy), then stops the sandbox,
// and writes one action failure row (sandbox_ref, lock_held_off_sec, sandbox_stopped). It publishes no
// terminal state: the run ends through stopKeptRun once the lock is free. A sandbox it could not stop
// is tried again on the next pass that is still held off.
func (s *Server) forceKeptStop(ctx context.Context, run types.AgentRun, action string) {
	now := s.cfg.Now()
	until, _ := s.keptUntil(run)
	h := keptEndHeldOff{until: until, since: now, last: now}
	if v, ok := s.keptEndHeldOff.Load(run.ID); ok {
		// The count goes on only for the same end refused on passes in a row; an end that moved (an
		// extend) or a gap longer than the limit starts it again.
		if prev := v.(keptEndHeldOff); prev.until.Equal(until) && now.Sub(prev.last) <= keptEndHoldOffLimit {
			h.since = prev.since
		} else {
			s.keptEndForced.Delete(run.ID)
		}
	}
	s.keptEndHeldOff.Store(run.ID, h)
	heldOff := now.Sub(h.since)
	if heldOff < keptEndHoldOffLimit {
		return
	}
	if _, done := s.keptEndForced.LoadOrStore(run.ID, struct{}{}); done {
		return
	}
	cur, err := s.cfg.Store.GetRun(ctx, run.ID)
	if err != nil || cur.State.IsTerminal() || cur.SandboxRef == "" {
		s.keptEndForced.Delete(run.ID)
		return
	}
	_ = s.stopProxyOrAudit(ctx, run.ID, cur.SandboxRef, action)
	stopped := s.stopSandboxOrAudit(ctx, run.ID, cur.SandboxRef, action)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, cur.SandboxRef, "failure",
		mustJSON(map[string]any{
			"sandbox_ref": cur.SandboxRef, "lock_held_off_sec": int64(heldOff.Seconds()), "sandbox_stopped": stopped,
		})))
	if !stopped {
		s.keptEndForced.Delete(run.ID)
	}
}

// reviveExpiryDue refuses a revive whose run reached the end it is kept until (keptUntil) while the
// revive got ready: the claim comes after the slow proxy image pull, and a revive that started inside
// the run's grace must not land past it. It reads the run again, as the claim's own guard would.
func (s *Server) reviveExpiryDue(ctx context.Context, id uuid.UUID) *reviveError {
	cur, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil || cur.LostAt == nil {
		// A failed read, or a run no longer lost, is the claim's to refuse (reasonReviveRunChanged).
		return nil
	}
	until, ok := s.keptUntil(cur)
	if !ok || s.cfg.Now().Before(until) {
		return nil
	}
	if cur.LostReason == types.LostEnded {
		return reviveRefused(http.StatusConflict, reasonReviveEndedFilesGone, "run has ended and its files are no longer kept; start a new run")
	}
	return reviveRefused(http.StatusConflict, reasonRevivePastEnd, "run has passed its end; extend it first")
}
