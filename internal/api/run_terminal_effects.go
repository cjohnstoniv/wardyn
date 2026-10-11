// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// finalizeRunEffects is shared by ordinary finalization and durable runner exit
// receipts, whose success audit already committed with their state transition.
func (s *Server) finalizeRunEffects(ctx context.Context, runID uuid.UUID, ref, action string, data map[string]any, finishAfterStop, gracefulStop bool) error {
	errs := []error{s.revokeRunCascade(ctx, runID), s.cancelRunApprovals(ctx, runID)}
	if !finishAfterStop {
		s.prepareRunOutput(ctx, runID, gracefulStop)
		s.finishRunOutput(ctx, runID)
	}
	teardownOK := false
	if ref != "" && s.cfg.Runner != nil {
		if serr := s.cfg.Runner.StopSandbox(ctx, ref); serr != nil {
			errs = append(errs, serr)
			td := make(map[string]any, len(data)+1)
			for k, v := range data {
				td[k] = v
			}
			td["teardown_error"] = serr.Error()
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
				ref, "failure", mustJSON(td)))
		} else {
			teardownOK = true
		}
	}
	if finishAfterStop {
		s.FinishRunOutput(ctx, runID)
	}
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
	// Clear the ref LAST, and only when this call's own StopSandbox succeeded.
	// LAST because reconcileRecordRun above still reads run.SandboxRef to tell
	// "the sandbox never started" from "capture was empty". Success-only because
	// the boot orphan sweep keys on a terminal run STILL carrying a ref — after
	// this line that means exactly "teardown failed", the case it exists for.
	// Kill/idle-stop tear down on their own paths and never clear; the evidence
	// endpoints' terminal-state guard covers those runs. A failed clear is
	// deliberately ignored: it degrades to the pre-clear behavior the boot
	// sweep already retries.
	if teardownOK && s.cfg.Store != nil {
		errs = append(errs, s.cfg.Store.SetSandboxRef(ctx, runID, ""))
	}
	return errors.Join(append(errs, ctx.Err())...)
}
