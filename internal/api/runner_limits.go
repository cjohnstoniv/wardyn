// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// machineNameStore records the host name a runner reports on connect.
type machineNameStore interface {
	SetRunnerMachineName(ctx context.Context, id uuid.UUID, name string) error
}

var _ machineNameStore = store.PG{}

// capsFromRunner keeps the machine name a runner's caps event reports, the latest winning. A name past
// types.MachineNameMax or with a control character is not stored, and neither it nor a failed write
// refuses the event: caps is acknowledged before the runner may become ready, and a machine name is a
// label, never something a run is admitted by.
func (s *Server) capsFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) {
	ms, ok := s.cfg.Store.(machineNameStore)
	var caps runnerwire.Caps
	if !ok || runnerwire.Unmarshal(ev.Data, &caps) != nil {
		return
	}
	if caps.MachineName = strings.TrimSpace(caps.MachineName); caps.MachineName == "" {
		return
	}
	if !types.ValidMachineName(caps.MachineName) {
		slog.WarnContext(ctx, "wardynd: runner reported an unusable machine name", slog.String("runner_id", runnerID.String()), slog.Int("bytes", len(caps.MachineName)))
		return
	}
	if err := ms.SetRunnerMachineName(ctx, runnerID, caps.MachineName); err != nil {
		slog.WarnContext(ctx, "wardynd: record runner machine name", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
	}
}

// handleUpdateMyRunner is PUT /api/v1/me/runners/{id}: a person changes their own runner's "Runs at
// once" (max_runs). Only the owner may: the machine is theirs, so administrators read max_runs on
// /runners and have no route that changes it. Another person's runner, a revoked one, a lapsed unclaimed
// one and an absent id answer alike. A raised limit lets waiting runs start at once; a lowered one ends
// no run.
func (s *Server) handleUpdateMyRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerSessionOwner(w, r, reasonRunnerSessionRequired, "sign in under your own account to change your runner")
	if !ok {
		return
	}
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerUpdateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.MaxRuns == nil || !types.ValidRunnerMaxRuns(*req.MaxRuns) {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerMaxRunsInvalid,
			"max_runs must be a whole number from 1 to "+strconv.Itoa(types.RunnerMaxRunsCeiling))
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	if err == nil && (row.Owner != owner || runnerLapsed(row, s.cfg.Now().UTC())) {
		err = store.ErrNotFound
	}
	var before int
	if err == nil {
		before, row, err = rs.UpdateRunnerMaxRuns(r.Context(), id, owner, *req.MaxRuns)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonRunnerNotFound, "runner not found")
		return
	}
	if err != nil {
		writeServerError(w, r, "update runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), owner, placement.ActionRunnerUpdate, id.String(), "success",
		mustJSON(map[string]any{"max_runs": map[string]int{"before": before, "after": row.MaxRuns}})))
	if row.MaxRuns > before {
		s.kickQueue()
	}
	if views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, false); ok {
		writeJSON(w, http.StatusOK, views[0])
	}
}
