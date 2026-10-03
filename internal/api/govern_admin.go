// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Constrained-admin mode (WARDYN_GOVERN_ADMIN_RUNS, docs/design/0.8/0.8.6-cadm.md).
// isOperator stays the route tier, credential namespace and run-reach predicate;
// runUngoverned is the narrower question every run seam asks: does this caller's
// run stand outside governance?

// governanceExemptKey carries the per-request exemption marker (the recording
// lane sets it in a later change). Nothing sets it yet.
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

// refuseNoTicketAttach is refuseAttachEntry's cookie/bearer lane door, called
// once mayEnterRun has passed: the admin token and local mode stay break-glass
// (runUngoverned), as does an operator-owned run.
func (s *Server) refuseNoTicketAttach(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	return !s.runUngoverned(r.Context()) && !run.OperatorOwned && s.refuseInteractiveAttach(w, r, run)
}
