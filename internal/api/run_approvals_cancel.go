// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// cancelRunApprovals is the approval half of the terminal cascade: every
// still-PENDING approval of a run that has just ended is moved to CANCELLED, so
// the operator's queue stops holding a decision nobody can act on and the
// console stops rendering live Approve/Deny buttons on a run it labels Killed.
//
// Nil-safe and best-effort like revokeRunCascade, and for the same reason: the
// run is already terminal, and a failed cancel must not gate the caller's own
// outcome. A failure is logged and audited (run.revoke/failure carries the
// detail) and returned for durable receipt cleanup; the stale-PENDING sweeper (ExpireStale) is
// still behind it as the backstop it has always been.
//
// The reason recorded on each row is derived from the run's state, read back
// from the store rather than passed in: finalizeRunTail's four callers do not
// all know which terminal state they landed on (the reconciler passes `to` in
// its audit data, the probe-reclaim path passes none), and the row is the
// authority on the transition that just won. One read per terminal transition.
func (s *Server) cancelRunApprovals(ctx context.Context, runID uuid.UUID) error {
	if s.cfg.Approvals == nil {
		return nil
	}
	// wardyn_credential_reauth_total{outcome="cancelled"} is a STATE-TRANSITION
	// counter, so it counts the credential_reauth rows the cancel actually MOVED
	// — not a pre-read of PENDING rows, which also counted one a human decided
	// between the read and the CAS (#151). Counting at a later resolve that
	// meets a terminal row counts retries instead, and never fires once the
	// sidecar has given up. AWS SSO re-auth rows only, the series' HELP: an
	// Azure DevOps sign-in or consent row is credential_reauth too, and is not.
	byKind, err := s.cfg.Approvals.CancelForRun(ctx, runID, s.terminalCancelReason(ctx, runID))
	for range byKind[approval.TallyReauthAWSSSO] {
		s.metrics.credentialReauthRecorded(credentialReauthOutcomeCancelled)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not cancel a terminal run's pending approvals",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.revoke",
			runID.String(), "failure", mustJSON(lifecycle.WithFailedSteps(map[string]any{"approval_cancel_error": err.Error()}))))
		return err
	}
	n := 0
	for _, c := range byKind {
		n += c
	}
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: cancelled a terminal run's pending approvals",
			slog.String("run_id", runID.String()), slog.Int("cancelled", n))
	}
	return ctx.Err()
}
