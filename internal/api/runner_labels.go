// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// PUT /runners/{id}/labels: a security operator's whole set of operator labels for a claimed runner, the only
// writer. A runner proposes labels at registration (runner_asserted), which are shown and never read by
// anything that records or decides: run.create, run.queue.start, the run read and the audit export carry the
// operator labels alone.

import (
	"context"
	"log/slog"
	"maps"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) handlePutRunnerLabels(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cfg.Store.(runnerTrustStore)
	ls, lok := s.cfg.Store.(store.RunnerLabelStore)
	if !ok || !lok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner labels require the Postgres store backend")
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerLabelsRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	if err := types.ValidateRunnerLabels(req.Labels); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerLabelsInvalid, "labels: "+err.Error())
		return
	}
	row, err := st.GetRunner(r.Context(), id)
	if err == nil && (row.State == types.RunnerRevoked || runnerLapsed(row, s.cfg.Now().UTC())) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "runner", reasonRunnerNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner", err)
		return
	}
	if row.State != types.RunnerClaimed {
		writeErrorReason(w, http.StatusConflict, reasonRunnerLabelsInvalid, "claim the runner first: only a claimed runner's labels are confirmed")
		return
	}
	before, err := ls.SetRunnerLabels(r.Context(), id, types.RunnerLabelsOperator, req.Labels, principalFromRequest(r), s.cfg.Now().UTC())
	if notFoundIf(w, err, "runner", reasonRunnerNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "write runner labels", err)
		return
	}
	if !maps.Equal(before, req.Labels) {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), placement.ActionRunnerLabelWrite, id.String(), "success",
			mustJSON(map[string]any{"before": emptyIfNil(before), "after": req.Labels})))
	}
	s.answerRunnerTrust(w, r, row)
}

// emptyIfNil is a label set as the audit row records it: {} for none, never null.
func emptyIfNil(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}

// runnerLabelsOf is ids' label sets, keyed by runner then source; nil, nil when the store keeps none.
func (s *Server) runnerLabelsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[types.RunnerLabelSource]store.RunnerLabelSet, error) {
	ls, ok := s.cfg.Store.(store.RunnerLabelStore)
	if !ok {
		return nil, nil
	}
	return ls.RunnerLabels(ctx, ids)
}

// applyRunnerLabels sets v's label fields from sets, the runner's rows by source. Who wrote the operator
// labels, and when, is the administrators' routes only.
func applyRunnerLabels(v *types.RunnerView, sets map[types.RunnerLabelSource]store.RunnerLabelSet, admin bool) {
	op := sets[types.RunnerLabelsOperator]
	v.Labels, v.AssertedLabels = op.Labels, sets[types.RunnerLabelsAsserted].Labels
	if admin && len(op.Labels) > 0 {
		at := op.CreatedAt
		v.LabelsSetBy, v.LabelsSetAt = op.CreatedBy, &at
	}
}

// stampRunnerLabels puts the run's runner's operator labels on a run.create or run.queue.start row as
// runner_labels. Only operator labels: a runner's own proposal never reaches the audit row. A failed read
// leaves the field out; the row records "no operator labels known", never a guess.
func (s *Server) stampRunnerLabels(ctx context.Context, run types.AgentRun, data map[string]any) {
	if run.RunnerID == nil {
		return
	}
	sets, err := s.runnerLabelsOf(ctx, []uuid.UUID{*run.RunnerID})
	if err != nil {
		slog.WarnContext(ctx, "read runner labels for the audit row", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if op := sets[*run.RunnerID][types.RunnerLabelsOperator]; len(op.Labels) > 0 {
		data["runner_labels"] = op.Labels
	}
}
