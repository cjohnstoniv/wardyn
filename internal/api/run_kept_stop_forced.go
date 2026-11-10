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

// keptEndHoldOffLimit is how long past its due end a kept run may wait on its run's lock (stopKeptRun)
// before forceKeptStop stops it without the lock. It is longer than every bounded hold of that lock (a pass
// back is at most computerFolderBackTimeout), so only a hold with no bound of its own reaches it.
const keptEndHoldOffLimit = 5 * time.Minute

// forceKeptStop is stopKeptRun's answer to a lock it could not take. Until the run's due end is
// keptEndHoldOffLimit past, it does nothing, and the next pass retries the end. The due end is read from
// the run's fresh row, so neither a slow sweep, a chain of lock holders nor a restart moves it; only a
// change to the stored end does (an extension, or a revive that claimed before expiry). Past it, the sweep
// stops the run's sandbox without the lock and without a pass back, so an expiry cannot be put off by a
// lock held for good: it cuts egress first (the proxy), then stops the sandbox, and writes one action
// failure row (sandbox_ref, past_due_sec, sandbox_stopped). It publishes no terminal state: the run ends
// through stopKeptRun once the lock is free. The stop is once per end and process; a sandbox it could not
// stop is tried again on the next pass that is still held off.
func (s *Server) forceKeptStop(ctx context.Context, run types.AgentRun, action string) {
	cur, err := s.cfg.Store.GetRun(ctx, run.ID) // the fresh row decides, never the listing's
	if err != nil || cur.State.IsTerminal() || cur.SandboxRef == "" {
		return
	}
	until, ok := s.keptUntil(cur)
	now := s.cfg.Now()
	if !ok || now.Before(until.Add(keptEndHoldOffLimit)) {
		return
	}
	if v, done := s.keptEndForced.Load(run.ID); done && v.(time.Time).Equal(until) {
		return
	}
	s.keptEndForced.Store(run.ID, until)
	_ = s.stopProxyOrAudit(ctx, run.ID, cur.SandboxRef, action)
	stopped := s.stopSandboxOrAudit(ctx, run.ID, cur.SandboxRef, action)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, cur.SandboxRef, "failure",
		mustJSON(map[string]any{"sandbox_ref": cur.SandboxRef, "past_due_sec": int64(now.Sub(until).Seconds()), "sandbox_stopped": stopped})))
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
