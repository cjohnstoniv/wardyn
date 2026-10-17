// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Constrained-admin mode (WARDYN_GOVERN_ADMIN_RUNS, docs/design/0.8/0.8.6-cadm.md).
// isOperator stays the route tier, credential namespace and run-reach predicate;
// runUngoverned is the narrower question every run seam asks: does this caller's
// run stand outside governance?

// governanceExemptKey carries the per-request exemption marker. Only
// handleRecordGoverned sets it, for WARDYN_GOVERN_ADMIN_RUNS_EXEMPT=recording.
type governanceExemptKey struct{}

func governanceExemptFromContext(ctx context.Context) bool {
	exempt, _ := ctx.Value(governanceExemptKey{}).(bool)
	return exempt
}

// runUngoverned reports whether the caller's runs stand outside governance. It
// is isOperator while the switch is off. With the switch on, an SSO admin's
// session and an admin-role personal token (both publish a person) are governed
// like a member; the admin token and local mode (no person to resolve a profile
// for) stay break-glass.
func (s *Server) runUngoverned(ctx context.Context) bool {
	return s.isOperator(ctx) &&
		(!s.cfg.GovernAdminRuns || oidcHumanFromContext(ctx) == "" || governanceExemptFromContext(ctx))
}

// presetLaunchOpenTo is presetOpenTo for the launch check: a governed admin
// launches only a preset open to their type. The list and read keep presetOpenTo
// because the admin's preset editor reads the same list.
func (s *Server) presetLaunchOpenTo(r *http.Request, p types.LaunchPreset) bool {
	return s.runUngoverned(r.Context()) || len(p.UserTypes) == 0 || slices.Contains(p.UserTypes, runCreatorUserType(r.Context()))
}

// adminDoorExempt reports whether a super admin's role snapshot may skip the
// run's deny_interactive / deny_ui_apps door. With the switch on only an
// operator-owned (break-glass) run is exempt: a governed admin's own run
// captured a profile, so the exemption would open the shell it closes.
func (s *Server) adminDoorExempt(run types.AgentRun) bool {
	return !s.cfg.GovernAdminRuns || run.OperatorOwned
}

// markGovernanceExempt stamps governance_exempt on a run.create datum when the
// switch is on and the run stands outside governance (the admin token or local
// mode). The key is absent otherwise, so a deployment without the switch writes
// the rows it always did.
func (s *Server) markGovernanceExempt(ctx context.Context, data map[string]any) {
	if s.cfg.GovernAdminRuns && s.runUngoverned(ctx) {
		data["governance_exempt"] = true
	}
}

// recordingGovernedRefusal is the 403 body of recording_governed, shown
// verbatim by the console (mock M10).
const recordingGovernedRefusal = "Record Mode is refused for admins whose runs are governed. Your operator can allow it with `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT=recording`."

// governAdminRunsExempts reports whether WARDYN_GOVERN_ADMIN_RUNS_EXEMPT names lane.
func (s *Server) governAdminRunsExempts(lane string) bool {
	return slices.Contains(s.cfg.GovernAdminRunsExempt, lane)
}

// handleRecordGoverned is the record route's handler: handleRecordWorkspace
// behind the recording_governed decision, which is made first, before the
// workspace read, the ceiling read and the import-step claim. With
// WARDYN_GOVERN_ADMIN_RUNS on, a caller whose runs are governed (an SSO admin,
// an admin-role personal token) is refused by name, because the lane builds its
// own allow-all, credentialed spec and skips the member clamp on purpose, and a
// profile would otherwise refuse it only by accident (record_ceiling_limit) or
// not at all for an unassigned admin. Under the recording exemption the request
// carries the exemption marker, which is what makes runUngoverned answer true
// inside this handler and nowhere else, and a fresh ceiling memo so nothing
// resolved earlier in the request under the governed answer survives. The admin
// token and local mode are already ungoverned and pass untouched. It sits beside
// the handler rather than inside it to keep the handler under the gocyclo gate.
func (s *Server) handleRecordGoverned(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GovernAdminRuns && !s.runUngoverned(r.Context()) {
		if !s.governAdminRunsExempts("recording") {
			s.refuse(w, r, authz.Deny(authz.ReasonRecordingGoverned, "workspaces.record", recordingGovernedRefusal))
			return
		}
		r = r.WithContext(context.WithValue(withCeilingMemo(r.Context()), governanceExemptKey{}, true))
	}
	s.handleRecordWorkspace(w, r)
}

// refuseNoTicketAttach is refuseAttachEntry's cookie/bearer lane door, called
// once mayEnterRun has passed: the admin token and local mode stay break-glass
// (runUngoverned), as does an operator-owned run.
func (s *Server) refuseNoTicketAttach(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	return !s.runUngoverned(r.Context()) && !run.OperatorOwned && s.refuseInteractiveAttach(w, r, run)
}
