// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
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

// handleUpdateMyRunner is PUT /api/v1/me/runners/{id}: a person declares their own runner's limits,
// the whole set (Runs at once, the total and the per-run CPU, memory and disk). Only the owner may: the
// machine is theirs and its limits only ever bound the owner's own runs, so administrators read them on
// /runners and have no route that changes them. Another person's runner, a revoked one, a lapsed
// unclaimed one and an absent id answer alike. Waiting runs are looked at again at once; a lowered
// limit ends no run.
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
	if req.Limits == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerLimitsInvalid, "limits are required: runs at once, total and per run")
		return
	}
	if err := req.Limits.Validate(types.RunnerMaxRunsCeiling); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerLimitsInvalid, err.Error())
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	if err == nil && (row.Owner != owner || runnerLapsed(row, s.cfg.Now().UTC())) {
		err = store.ErrNotFound
	}
	if err == nil {
		if msg := s.pastReported(row, *req.Limits); msg != "" {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerLimitsInvalid, msg)
			return
		}
	}
	var before *types.RunnerLimits
	if err == nil {
		before, row, err = rs.UpdateRunnerLimits(r.Context(), id, owner, *req.Limits)
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
		mustJSON(map[string]any{"limits": map[string]*types.RunnerLimits{"before": before, "after": row.Limits}})))
	s.kickQueue()
	if views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, false); ok {
		writeJSON(w, http.StatusOK, views[0])
	}
}

// pastReported refuses a total past what the runner last reported it has, where this replica holds its
// report: a runner never offers more than its computer has. "" when the totals fit or nothing is known.
func (s *Server) pastReported(row types.Runner, l types.RunnerLimits) string {
	caps, _, _ := s.runnerReportedCaps(row.ID)
	switch {
	case caps == nil:
		return ""
	case l.Total.CPUMillis > caps.CPUMillisMax:
		return fmt.Sprintf("Not allowed: %s reports %s CPUs, so its total can't be higher.", row.Name, strconv.FormatFloat(float64(caps.CPUMillisMax)/1000, 'f', -1, 64))
	case l.Total.MemoryMiB > caps.MemoryMiBMax:
		return fmt.Sprintf("Not allowed: %s reports %s GB memory, so its total can't be higher.", row.Name, strings.TrimSuffix(strconv.FormatFloat(float64(caps.MemoryMiBMax)/1024, 'f', 1, 64), ".0"))
	}
	return ""
}
