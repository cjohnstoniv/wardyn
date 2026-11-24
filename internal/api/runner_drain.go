// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's drain (the runner_drains table): an administrator sets it before maintenance, and the runner then
// takes no new sandbox (runner_draining in placement and in a lost run's revive). Its running runs are not touched.

// runnerDrainStoreOf is the store's drain and trust-read capability, or writes the 501 a store without them gets.
func (s *Server) runnerDrainStoreOf(w http.ResponseWriter) (store.RunnerDrainStore, runnerTrustStore, bool) {
	ds, okDrain := s.cfg.Store.(store.RunnerDrainStore)
	ts, okTrust := s.cfg.Store.(runnerTrustStore)
	if !okDrain || !okTrust {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner drains require the Postgres store backend")
		return nil, nil, false
	}
	return ds, ts, true
}

// readDrainableRunner reads the runner a drain route names: a revoked or lapsed runner is not found.
func (s *Server) readDrainableRunner(w http.ResponseWriter, r *http.Request, ts runnerTrustStore, id uuid.UUID) (types.Runner, bool) {
	row, err := ts.GetRunner(r.Context(), id)
	if err == nil && (row.State == types.RunnerRevoked || runnerLapsed(row, s.cfg.Now().UTC())) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "runner", reasonRunnerNotFound) {
		return types.Runner{}, false
	}
	if err != nil {
		writeServerError(w, r, "read runner", err)
		return types.Runner{}, false
	}
	return row, true
}

// runnerRunning is how many runs hold a sandbox on the runner now (its In use runs), read without a lock.
func (s *Server) runnerRunning(w http.ResponseWriter, r *http.Request, id uuid.UUID) (int, bool) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return 0, false
	}
	used, err := rs.RunnerUsage(r.Context(), []uuid.UUID{id}, nil, uuid.Nil)
	if err != nil {
		writeServerError(w, r, "read runner usage", err)
		return 0, false
	}
	return used[id.String()].Runs, true
}

// answerRunnerDrain answers the runner as the administrators' routes show it.
func (s *Server) answerRunnerDrain(w http.ResponseWriter, r *http.Request, row types.Runner) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	if views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, true); ok {
		writeJSON(w, http.StatusOK, views[0])
	}
}

// handlePutRunnerDrain is PUT /api/v1/runners/{id}/drain (administrators): the runner takes no new sandbox.
func (s *Server) handlePutRunnerDrain(w http.ResponseWriter, r *http.Request) {
	ds, ts, ok := s.runnerDrainStoreOf(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerDrainRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxGovernanceRejectReasonLen || !controlCharFree(reason) {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerDrainInvalid, "reason is required: at most 512 characters, without control characters")
		return
	}
	row, ok := s.readDrainableRunner(w, r, ts, id)
	if !ok {
		return
	}
	if row.State != types.RunnerClaimed {
		writeErrorReason(w, http.StatusConflict, reasonRunnerDrainInvalid, "claim the runner first: only a claimed runner takes runs")
		return
	}
	running, ok := s.runnerRunning(w, r, id)
	if !ok {
		return
	}
	_, set, err := ds.DrainRunner(r.Context(), id, types.RunnerDrain{CreatedBy: principalFromRequest(r), CreatedAt: s.cfg.Now().UTC(), Reason: reason})
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusConflict, reasonRunnerDrainInvalid, "claim the runner first: only a claimed runner takes runs")
		return
	}
	if err != nil {
		writeServerError(w, r, "drain runner", err)
		return
	}
	if set {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), placement.ActionRunnerDrain, id.String(), "success",
			mustJSON(map[string]any{"reason": reason, "running": running})))
	}
	s.answerRunnerDrain(w, r, row)
}

// handleDeleteRunnerDrain is DELETE /api/v1/runners/{id}/drain (administrators): the runner takes runs again. Its
// audit row carries no reason: the text was the drainer's, and this row's actor may be another administrator.
func (s *Server) handleDeleteRunnerDrain(w http.ResponseWriter, r *http.Request) {
	ds, ts, ok := s.runnerDrainStoreOf(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	row, ok := s.readDrainableRunner(w, r, ts, id)
	if !ok {
		return
	}
	running, ok := s.runnerRunning(w, r, id)
	if !ok {
		return
	}
	was, cleared, err := ds.UndrainRunner(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "undrain runner", err)
		return
	}
	if cleared {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), placement.ActionRunnerUndrain, id.String(), "success",
			mustJSON(map[string]any{"running": running, "drained_for_sec": int64(s.cfg.Now().UTC().Sub(was.CreatedAt) / time.Second)})))
	}
	s.answerRunnerDrain(w, r, row)
}

// runnerDrainsOf is the drain of each of ids, by runner; a runner with none is absent. nil, nil from a store that
// keeps none (a test fake): every runner then reads as not drained.
func (s *Server) runnerDrainsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*types.RunnerDrain, error) {
	ds, ok := s.cfg.Store.(store.RunnerDrainStore)
	if !ok {
		return nil, nil
	}
	rows, err := ds.RunnerDrains(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*types.RunnerDrain, len(rows))
	for id, d := range rows {
		d := d
		out[id] = &d
	}
	return out, nil
}

// reviveDrainRefusal refuses a lost run's revive on a runner an administrator drained: a revive restarts
// the run's proxy and agent on that runner, which takes no new work while it is drained. A live run's
// proxy refresh is a running run's and is not refused.
func (s *Server) reviveDrainRefusal(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) *reviveError {
	if run.RunnerID == nil || run.LostAt == nil {
		return nil
	}
	drains, err := s.runnerDrainsOf(ctx, []uuid.UUID{*run.RunnerID})
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveClaimFailed, loggedMsg(ctx, "read runner drains", err))
	}
	if drains[*run.RunnerID] == nil {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": string(placement.ReasonRunnerDraining), "runner_id": run.RunnerID})))
	return reviveRefused(placement.ReasonRunnerDraining.Status(), string(placement.ReasonRunnerDraining),
		"This run's runner is drained for maintenance, so the run cannot be revived there now. Revive it once the runner is undrained.")
}
