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
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerCapsStore records a session's caps report on the runner row, fenced to that session.
type runnerCapsStore interface {
	RecordRunnerCaps(ctx context.Context, id uuid.UUID, session string, r store.RunnerCapsReport, now time.Time) (bool, error)
}

var _ runnerCapsStore = store.PG{}

// capsFromRunner keeps the version, the machine name, the architecture and the retire capability a
// runner's caps event reports, the latest session winning: a superseded session's late caps write
// nothing (store.PG.RecordRunnerCaps). A version or name past types.MachineNameMax or with a control
// character, or an architecture that is not types.ValidArch, is not stored, and neither it nor a failed
// write refuses the event: caps is acknowledged before the runner may become ready, and all are labels,
// never something a run is admitted by. A runner registered before it reported an architecture gets
// one on its next connect. Every caps event sets the retire capability: a runner without caps.retire
// (older than 0.9.3) is never sent a retire_run. The runner.connect row Connected owed the session is
// written last, so it carries the version. The offline summary and relay status are kept apart
// (healthFromRunner).
func (s *Server) capsFromRunner(ctx context.Context, runnerID uuid.UUID, session string, ev runnerwire.Event) {
	var caps runnerwire.Caps
	if runnerwire.Unmarshal(ev.Data, &caps) != nil {
		return
	}
	r := store.RunnerCapsReport{Version: strings.TrimSpace(caps.Version), MachineName: strings.TrimSpace(caps.MachineName), Arch: caps.Arch, RetireCapable: caps.Retire}
	if !types.ValidMachineName(r.Version) {
		slog.WarnContext(ctx, "wardynd: runner reported an unusable version", slog.String("runner_id", runnerID.String()), slog.Int("bytes", len(r.Version)))
		r.Version = ""
	}
	if !types.ValidMachineName(r.MachineName) {
		slog.WarnContext(ctx, "wardynd: runner reported an unusable machine name", slog.String("runner_id", runnerID.String()), slog.Int("bytes", len(r.MachineName)))
		r.MachineName = ""
	}
	if !types.ValidArch(r.Arch) {
		slog.WarnContext(ctx, "wardynd: runner reported an unknown architecture", slog.String("runner_id", runnerID.String()), slog.String("arch", strings.ToValidUTF8(r.Arch, "?")))
		r.Arch = ""
	}
	if cs, ok := s.cfg.Store.(runnerCapsStore); ok {
		if _, err := cs.RecordRunnerCaps(ctx, runnerID, session, r, s.cfg.Now()); err != nil {
			slog.WarnContext(ctx, "wardynd: record runner caps", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
		}
	}
	s.healthFromRunner(ctx, runnerID, caps)
	if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
		s.writeOwedRunnerConnect(ctx, rs, runnerID, session, r.Version)
	}
}

// handleUpdateMyRunner is PUT /api/v1/me/runners/{id}: a person declares their own runner's limits,
// the whole set (Runs at once, the total and the per-run CPU, memory and disk). Only the owner may: the
// machine is theirs and its limits only ever bound the owner's own runs, so administrators read them on
// /runners and have no route that changes them. Another person's runner, an organization's, a revoked
// one, a lapsed unclaimed one and an absent id answer alike. Waiting runs are looked at again at once; a
// lowered limit ends no run.
func (s *Server) handleUpdateMyRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerSessionOwner(w, r, reasonRunnerSessionRequired, "sign in under your own account to change your runner")
	if !ok {
		return
	}
	s.updateRunnerLimits(w, r, types.RunnerOwnerPerson, owner, owner)
}

// handleUpdateOrgRunner is PUT /api/v1/runners/{id}: a security operator declares an organization's
// runner's limits, which no person owns (chart values apply only at register). A person's runner stays
// its owner's to change and answers here as not found, like a revoked, lapsed or absent one.
func (s *Server) handleUpdateOrgRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	s.updateRunnerLimits(w, r, types.RunnerOwnerOrg, "", principalFromRequest(r))
}

// updateRunnerLimits writes the limits of the runner of kind owned by owner ("" for an org runner), as actor.
func (s *Server) updateRunnerLimits(w http.ResponseWriter, r *http.Request, kind types.RunnerOwnerKind, owner, actor string) {
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
	if err == nil && (row.Owner != owner || (row.OwnerKind == types.RunnerOwnerOrg) != (kind == types.RunnerOwnerOrg) || runnerLapsed(row, s.cfg.Now().UTC())) {
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
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), actor, placement.ActionRunnerUpdate, id.String(), "success",
		mustJSON(map[string]any{"limits": map[string]*types.RunnerLimits{"before": before, "after": row.Limits}})))
	s.kickQueue()
	if views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, kind == types.RunnerOwnerOrg); ok {
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
