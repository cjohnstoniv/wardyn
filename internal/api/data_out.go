// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// data_out is one rule over every path a run's content leaves by (docs/POLICIES.md#data_out). The rule is
// frozen into the run's policy envelope at create and re-met at the seal; the doors below read it from the
// envelope, as the desktop rules are read.

const (
	dataOutKeepsOutputMsg = "This run's data_out rule keeps its content in: only a security admin can read its output."
	dataOutAppsMsg        = "This run's data_out rule keeps content in, so its web apps are off; its desktop still opens."
	dataOutUnreadableMsg  = "This run's data_out rule could not be read. Try again."
	dataOutFolderMsg      = "This run's data_out rule keeps content in, so its computer folder cannot come back."
	// dataOutReviewUnavailableMsg answers the Files panel on a machine that cannot move files through Wardyn.
	dataOutReviewUnavailableMsg = "This run's machine cannot move files through Wardyn; nothing can leave it while data_out is review."
)

// validateDataOut refuses a data_out value that is not a rule.
func validateDataOut(r types.TransferRule) error {
	if !types.ValidDataOut(r) {
		return fmt.Errorf("data_out: %q is not a rule; want allowed, review or blocked", r)
	}
	return nil
}

// runDataOutRule is the run's data_out rule from its policy envelope, "" read as allowed. found is false for a
// run that recorded no envelope, which carries no rule.
func (s *Server) runDataOutRule(ctx context.Context, runID uuid.UUID) (rule types.TransferRule, found bool, err error) {
	envelope, _, found, err := s.runEffectivePolicy(ctx, runID)
	if err != nil {
		return types.TransferAllowed, false, err
	}
	if !found {
		// A run with no recorded policy (its audit rows were pruned) carries no rule; refusing it would close output and replay on every pruned run.
		return types.TransferAllowed, false, nil
	}
	return envelope.DataOut.DataOut(), true, nil
}

// dataOutHolds is the rule that keeps the run's content in, and whether it does. A rule that cannot be read
// holds as blocked.
func (s *Server) dataOutHolds(ctx context.Context, runID uuid.UUID) (types.TransferRule, bool) {
	rule, _, err := s.runDataOutRule(ctx, runID)
	if err != nil {
		return types.TransferBlocked, true
	}
	return rule, rule != types.TransferAllowed
}

// runDataOutAllowed reports whether the run's content may leave by its own doors: its rule is allowed (or it
// has none), and a rule that cannot be read is not allowed.
func (s *Server) runDataOutAllowed(ctx context.Context, runID uuid.UUID) bool {
	_, held := s.dataOutHolds(ctx, runID)
	return !held
}

// auditDataOutDenied records one path a run's data_out rule kept content in on.
func (s *Server) auditDataOutDenied(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor, path string, rule types.TransferRule) {
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.data_out.deny", runID.String(), "denied",
		mustJSON(map[string]any{"path": path, "rule": string(rule)})))
}

// refuseDataOutUnreadable answers 503 for a rule that could not be read.
func refuseDataOutUnreadable(ctx context.Context, w http.ResponseWriter, runID uuid.UUID, err error) {
	slog.WarnContext(ctx, "wardynd: a run's data_out rule is unreadable; the path is refused", "run_id", runID, "err", err)
	writeErrorReason(w, http.StatusServiceUnavailable, reasonDataOutUnreadable, dataOutUnreadableMsg)
}

// refuseDataOutReader keeps a run's content from a reader who is not a security admin when its rule is review
// or blocked. path names the door in the audit row. It answers and returns true on a refusal.
func (s *Server) refuseDataOutReader(w http.ResponseWriter, r *http.Request, run types.AgentRun, path string, security bool) bool {
	if security {
		return false
	}
	rule, _, err := s.runDataOutRule(r.Context(), run.ID)
	if err != nil {
		refuseDataOutUnreadable(r.Context(), w, run.ID, err)
		return true
	}
	if rule == types.TransferAllowed {
		return false
	}
	at, actor := actorFromRequest(r)
	s.auditDataOutDenied(r.Context(), run.ID, at, actor, path, rule)
	writeErrorReason(w, http.StatusForbidden, reasonDataOutBlocked, dataOutKeepsOutputMsg)
	return true
}

// refuseAppDataOut refuses a web app other than the desktop under a run whose rule is review or blocked: the
// desktop is a display, and any other app can carry content out.
func (s *Server) refuseAppDataOut(w http.ResponseWriter, r *http.Request, run types.AgentRun, app string) bool {
	if uiDesktopApp(app) {
		return false
	}
	rule, _, err := s.runDataOutRule(r.Context(), run.ID)
	if err != nil {
		refuseDataOutUnreadable(r.Context(), w, run.ID, err)
		return true
	}
	if rule == types.TransferAllowed {
		return false
	}
	at, actor := actorFromRequest(r)
	s.auditDataOutDenied(r.Context(), run.ID, at, actor, "app", rule)
	writeErrorReason(w, http.StatusForbidden, reasonDataOutBlocked, dataOutAppsMsg)
	return true
}

// refuseReviewUnavailable answers the Files panel's exec error err when the machine cannot move files at all
// and the run's rule is review: nothing can leave it, and the person is told so in place of a transfer failure.
func (s *Server) refuseReviewUnavailable(w http.ResponseWriter, r *http.Request, run types.AgentRun, err error) bool {
	if !errors.Is(err, runner.ErrExecStreamUnsupported) {
		return false
	}
	if rule, _, rerr := s.runDataOutRule(r.Context(), run.ID); rerr != nil || rule != types.TransferReview {
		return false
	}
	writeErrorReason(w, http.StatusConflict, reasonDataOutReviewUnavailable, dataOutReviewUnavailableMsg)
	return true
}

// stepComputerFolderDataOut refuses a computer folder that syncs back (direction both) under a rule that keeps
// content in. It runs after stepPolicy, so the spec is the clamped one, and after local_folder was normalised.
func (s *Server) stepComputerFolderDataOut(f *runFold, _ *foldRecorder) bool {
	lf := f.req.LocalFolder
	if lf == nil || lf.RunnerID == nil || lf.Direction != types.ComputerFolderBoth || f.spec.DataOut.DataOut() == types.TransferAllowed {
		return true
	}
	return !runError(http.StatusUnprocessableEntity, reasonComputerFolderDataOut,
		"local_folder.direction: this run's data_out rule keeps content in, so a computer folder can only go in; set direction to in").write(s, f.w, f.r)
}
