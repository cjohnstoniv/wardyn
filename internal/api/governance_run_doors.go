// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance limits that reach past create: deny_ui_apps strips ui_apps
// at create and closes the UI gateway (#1391), deny_interactive closes the
// terminal attach and the SSH gateway (#1392). The create-path refusals are
// denyUserGovernance's.
//
// The gateway doors key on the profile the RUN was created under, read as it
// stands now so a limit an admin sets later reaches runs already going, and
// never on the caller's ceiling: only the owner or a super admin gets this
// far, and the SSH lane has no session to resolve a ceiling from. A super
// admin is exempt at every door, as at create (effectiveCeiling resolves no
// profile for an operator).

// runProfile is the governance profile run was created under, as it stands
// now and composed from its chain (governance_compose.go): a child sees what its base
// carries today, so an edit to the base reaches runs already going. nil when the run captured none
// (an unassigned or super-admin owner) or the profile has since been deleted, which leaves nothing
// to bind — the run-limits reclamp reads a deleted profile the same way. An error is returned,
// never read as "no profile": a base that cannot be read, a chain that loops or runs deeper than
// three, and a composition nothing satisfies all close the door. ownerProfile (revive) refuses a
// deleted profile instead, since a revive re-issues authority while a door only narrows a session;
// the two are left apart on purpose.
func (s *Server) runProfile(ctx context.Context, run types.AgentRun) (*ResolvedProfile, error) {
	if run.GovernanceProfileID == nil {
		return nil, nil
	}
	p, err := s.resolveProfileByID(ctx, *run.GovernanceProfileID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("resolve the run's governance profile: %w", err)
	}
	return p, nil
}

// refuseRunProfileError answers a door whose profile read failed: a composition nothing satisfies is
// the audited authz refusal governance_overlay_unsatisfiable (naming the run's own profile only),
// anything else a 500, and either way the door stays shut. True when it has answered.
func (s *Server) refuseRunProfileError(w http.ResponseWriter, r *http.Request, target, op string, run types.AgentRun, err error) bool {
	if u, ok := isOverlayUnsatisfiable(err); ok {
		return s.refuse(w, r, authz.Deny(authz.ReasonGovernanceOverlayUnsatisfiable, target, u.Error()).OnRun(run.ID))
	}
	writeServerError(w, r, op, err)
	return true
}

// interactiveDeniedProfile names the profile whose deny_interactive closes a
// live session into run — a terminal attach or an SSH connection — or ""
// when none does. The create path already refuses an interactive run under
// the limit; this closes the shell a person could still open in a task or exec
// run. The harness sign-in run is exempt, as harnessLoginGovernance exempts it
// at create: its terminal is a device-code step, not the member's session.
func (s *Server) interactiveDeniedProfile(ctx context.Context, run types.AgentRun) (string, error) {
	p, err := s.interactiveDeniedBy(ctx, run)
	if err != nil || p == nil {
		return "", err
	}
	return p.Name, nil
}

// interactiveDeniedBy is interactiveDeniedProfile's profile itself, nil when no
// profile closes the session, so the attach refusal can name the policy as well.
func (s *Server) interactiveDeniedBy(ctx context.Context, run types.AgentRun) (*ResolvedProfile, error) {
	if run.Task == harnessLoginTask {
		return nil, nil
	}
	p, err := s.runProfile(ctx, run)
	if err != nil || p == nil || !p.Limits.DenyInteractive {
		return nil, err
	}
	return p, nil
}

// refuseInteractiveAttach is the terminal attach's deny_interactive door: true
// when it has answered. Callers skip it for a super admin.
func (s *Server) refuseInteractiveAttach(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	p, err := s.interactiveDeniedBy(r.Context(), run)
	if err != nil {
		return s.refuseRunProfileError(w, r, "runs.attach", "attach", run, err)
	}
	if p == nil {
		return false
	}
	return s.refuse(w, r, authz.Deny(authz.ReasonGovernanceProfile, "runs.attach", fmt.Sprintf(
		"attaching is not allowed under the governance profile %q this run was launched under: it denies interactive sessions.", p.Name)).OnRun(run.ID).WithPolicy(profilePolicyRef(p)))
}

// refuseUIAppsDenied is the UI gateway's deny_ui_apps door, for a run created
// before the limit was set (a run created after it carries no ui_apps at all,
// boundUIApps): true when it has answered. Callers skip it for a super admin.
func (s *Server) refuseUIAppsDenied(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	p, err := s.runProfile(r.Context(), run)
	if err != nil {
		return s.refuseRunProfileError(w, r, "runs.ui_apps", "ui gateway", run, err)
	}
	if p == nil || !p.Limits.DenyUIApps {
		return false
	}
	return s.refuse(w, r, authz.Deny(authz.ReasonGovernanceProfile, "runs.ui_apps", fmt.Sprintf(
		"UI apps are not allowed under the governance profile %q this run was launched under.", p.Name)).OnRun(run.ID).WithPolicy(profilePolicyRef(p)))
}

// boundUIApps is deny_ui_apps at create, called from both of resolveRunPolicy's
// arms like boundResources, since composer.Clamp never sees the default
// arm's spec. It binds whoever the ceiling's limits bind: an assigned member or
// security admin, never an operator. Audited as a drop unless dryRun.
func (s *Server) boundUIApps(ctx context.Context, r *http.Request, spec *types.RunPolicySpec, ceiling governanceCeiling, dryRun bool) []string {
	if ceiling.Profile == nil || !ceiling.Limits.DenyUIApps || len(spec.UIApps) == 0 || s.runUngoverned(r.Context()) {
		return nil
	}
	dropped := make([]string, 0, len(spec.UIApps))
	for _, a := range spec.UIApps {
		dropped = append(dropped, fmt.Sprintf("%s:%d", a.Name, a.Port))
	}
	spec.UIApps = nil
	if !dryRun {
		s.recordRefusal(ctx, r, authz.Drop(authz.ReasonGovernanceProfile, "runs.ui_apps", dropped))
	}
	return []string{fmt.Sprintf("dropped %d ui_app(s) denied by governance profile %q: %s",
		len(dropped), ceiling.Profile.Name, strings.Join(dropped, ","))}
}
