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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// data_out is one rule over every path a run's content leaves by (docs/POLICIES.md#data_out). The rule is
// frozen into the run's policy envelope at create and re-met at the seal. Dispatch keeps it on the run's row
// too, where audit retention cannot prune it, and the doors below read it from there (runDataOutRule).

const (
	dataOutKeepsOutputMsg = "This run's data_out rule keeps its content in: only a security admin can read its output."
	dataOutRecordingMsg   = "This run's data_out rule keeps its content in: only a super admin can replay its recording."
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

// errDataOutGenerationUnread is a run's data_out rule that cannot be told because its current policy
// generation's row cannot be read (runEffectivePolicyGen). It is an error and never a rule: the rule is at least
// the resolved one, whose value is no longer enforced, so no answer is safe, and the callers that treat a read
// error as unreadable (a 503 at the doors, blocked on SSH, the kept sandbox at a run's end) already do the right
// thing. Answering blocked instead would stop an ended run's sandbox and lose its computer folder's changes.
var errDataOutGenerationUnread = errors.New("the run's current policy generation could not be read")

// runDataOutRule is the run's data_out rule. Its row's rule comes first (agent_runs.data_out, written at
// dispatch before the run.policy.resolve row and raised by a revive), which audit retention cannot prune. A
// row with none reads its policy envelope, "" read as allowed: a run from before the column that a previous
// release dispatched after the upgrade's migration. Such a run whose current policy generation's row cannot be
// read has no rule to give, and is an error wrapping errDataOutGenerationUnread with the generation; a run
// with a rule on its row never reads the generation, since a revive raises the row before its generation is
// current. found is false when neither holds a rule, and the rule is then allowed, as for a run never
// dispatched: dispatch writes the row before any sandbox exists, so no content was kept in. A run from before
// the column that was dispatched but whose policy rows retention had pruned gets its row set to blocked by the
// migration (0292), so it never reaches this answer.
func (s *Server) runDataOutRule(ctx context.Context, runID uuid.UUID) (rule types.TransferRule, found bool, err error) {
	stored, _, err := s.storedRunDataOut(ctx, runID)
	if err != nil {
		return types.TransferAllowed, false, err
	}
	if stored != nil {
		return *stored, true, nil
	}
	envelope, _, found, unread, err := s.runEffectivePolicyGen(ctx, runID)
	if err != nil {
		return types.TransferAllowed, false, err
	}
	if unread > 0 {
		return types.TransferAllowed, false, fmt.Errorf("%w (generation %d)", errDataOutGenerationUnread, unread)
	}
	if !found {
		return types.TransferAllowed, false, nil
	}
	return envelope.DataOut.DataOut(), true, nil
}

// dataOutUnrecorded is the rule a revive or an in-place edit carries for a run whose row and audit log both
// hold none on a store that keeps the column (reviveCarryRecordedRules). Such a run has a sandbox, so it was
// dispatched, but by a previous release that kept no row, and retention has since pruned its policy rows: its
// rule is unknown, so the new generation keeps its content in rather than open what the run may have kept.
const dataOutUnrecorded = types.TransferBlocked

// storedRunDataOut is the rule the run's row keeps, nil when it keeps none, and whether the store keeps the
// column at all (store.RunDataOuts).
func (s *Server) storedRunDataOut(ctx context.Context, runID uuid.UUID) (rule *types.TransferRule, keeps bool, err error) {
	if _, ok := s.cfg.Store.(store.RunDataOuts); !ok {
		return nil, false, nil
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, true, err
	}
	return run.DataOut, true, nil
}

// raiseRunDataOut stores rule on the run's row, or keeps the stricter rule it holds (store.RunDataOuts). A
// store without the column has nothing to write.
func (s *Server) raiseRunDataOut(ctx context.Context, runID uuid.UUID, rule types.TransferRule) error {
	st, ok := s.cfg.Store.(store.RunDataOuts)
	if !ok {
		return nil
	}
	return st.RaiseRunDataOut(ctx, runID, rule)
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
// or blocked. path names the door in the audit row and msg is the sentence a held rule answers with. It answers
// and returns true on a refusal.
func (s *Server) refuseDataOutReader(w http.ResponseWriter, r *http.Request, run types.AgentRun, path, msg string, security bool) bool {
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
	writeErrorReason(w, http.StatusForbidden, reasonDataOutBlocked, msg)
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
