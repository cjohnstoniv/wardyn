// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// forceUnkeptHeld is leaseRun's answer to a kept-run visit whose lock is held. A run whose target is known
// not to keep its sandbox has nothing to wait for but the lock: the visit ends it through stopKeptRun, whose
// held path (forceKeptStop) takes the end without the lock keptEndHoldOffLimit after the run was lost, not
// after its keep grace. A target that keeps, or one not yet known, is left to the grace and the next pass,
// as before.
func (s *Server) forceUnkeptHeld(ctx context.Context, leaser store.RunLeaser, run types.AgentRun) {
	keeps, known := s.endKeepsRunTarget(ctx, run)
	if !known || keeps {
		return
	}
	if run.LostReason == types.LostEnded {
		s.stopKeptRun(ctx, leaser, run, types.RunStopped, "run.ended", map[string]any{
			"kept": false, "end_error": runner.ErrEndUnsupported.Error(), "ended_at": run.LostAt,
		})
		return
	}
	s.stopKeptRun(ctx, leaser, run, types.RunFailed, "run.lost", map[string]any{
		"kept": false, "lost_error": runner.ErrEndUnsupported.Error(), "reason": string(run.LostReason),
	})
}

// forcedResumeRowWritten holds, per server and run, the retry's resumed row this process has written. In
// memory only, like teardownRowsWritten: a restart writes its own, which is the case it reports.
var forcedResumeRowWritten sync.Map

type forcedResumeKey struct {
	srv *Server
	run uuid.UUID
}

// forcedCutObserved reports a cut of the agent that was seen to hold: cutAgent answered no error and fenced
// the sandbox, or stopped the proxy of a server-hosted one. A proxy stop on a runner without the fence is not
// seen to cut the agent off, and is not observed.
func forcedCutObserved(run types.AgentRun, kind cutKind, err error) bool {
	return err == nil && (kind == cutFenced || (run.RunnerID == nil && kind == cutProxyStopped))
}

// auditCutFailure writes the failure row a cut that failed leaves under action, as stopProxyOrAudit wrote it.
func (s *Server) auditCutFailure(ctx context.Context, runID uuid.UUID, ref, action string, err error) {
	s.teardownOrAudit(ctx, runID, ref, action, func(context.Context, string) error { return err })
}
