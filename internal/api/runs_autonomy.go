// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveRunAutonomy is THE autonomy gate (0.8 #97): one function, called from
// both doors — handleCreateRun right after resolveEnforcedConfinement, and
// handlePreflightRun right after enforcedConfinement — so the level Review
// shows is the level launch enforces, by construction.
//
// ONE gate rather than two, because the alternative has a name: if Review can
// say a run is permitted and launch then refuses it, the rubric is decoration.
// TestPreflightMirrorsLaunchGates sees this as a launch gate and requires the
// preflight call; what it cannot see is that both calls fold the SAME inputs,
// which is why the arithmetic is a pure package function
// (composer.AutonomyPostureOf / composer.FoldAutonomy) and everything
// door-specific — the 403s, the derived field, the 201 warnings — is here.
//
// It MUTATES req.ToolApprovals, and the launch call site is chosen for it: the
// write has to land before createRunAuditData reads the request and before the
// dispatchParams literal is built, or the audit row and the sandbox env would
// say `auto` while the resolution said `hold`.
//
// Returns ok=false once it has written its own 403 (denyMemberField, the
// existing member-refusal shape carrying the existing `governance_profile`
// reason — the closed reason enum stays closed). The warnings ride the 201.
func (s *Server) resolveRunAutonomy(w http.ResponseWriter, r *http.Request, req *createRunRequest,
	spec types.RunPolicySpec, wsRefs []types.Workspace, enforced types.ConfinementClass,
	ceiling governanceCeiling,
) (types.AutonomyResolution, []string, bool) {
	// No profile, or a profile with no rubric: the zero value and no bound. An
	// UNASSIGNED member — and every operator — is byte-for-byte what they were
	// before this gate existed, the same absent-row rule every other
	// GovernanceLimits field follows. Nothing below runs.
	if ceiling.Profile == nil || ceiling.Limits.AutonomyRubric == nil {
		return types.AutonomyResolution{}, nil, true
	}
	posture := composer.AutonomyPostureOf(s.autonomyPostureSpec(r.Context(), spec, wsRefs, req.Repo), enforced)
	level, boundBy := composer.FoldAutonomy(*ceiling.Limits.AutonomyRubric, posture)
	res := types.AutonomyResolution{Level: level, Posture: posture, BoundBy: boundBy}
	// An all-unset rubric — or one that leaves this posture's three fields
	// unset — caps nothing, identically to a nil rubric (AutonomyRubric's own
	// doc). The posture still travels, so the audit row and Review record what
	// was graded even when nothing bound it.
	if level == "" {
		return res, nil, true
	}
	name := ceiling.Profile.Name
	// Rendered ONCE and passed down, so the refusals and the derived-hold
	// warning cannot drift into naming different causes for one resolution.
	bound := autonomyBoundList(boundBy)
	// The ladder, expressed as the LOWEST level that permits each capability
	// rather than one arm per rung. Separate arms are how a hole gets shipped:
	// L0 is the most supervised rung, so anything L1 refuses it must refuse
	// too, and two independently written arms drift the moment one grows a case.
	//
	//	task_mode=exec    L3   the door that routes around every other gate
	//	seed_auto_tools   L2   the pre-attach span runs before any human is at the pane
	//	non-interactive   L1   unattended at all
	if level.Rank() < types.AutonomyL3.Rank() && req.TaskMode == "exec" {
		s.denyMemberField(w, r, "runs.task_mode", "governance_profile", fmt.Sprintf(
			"`task_mode=exec` is not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"and an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead.",
			name, level, bound))
		return res, nil, false
	}
	if level.Rank() < types.AutonomyL2.Rank() && req.SeedAutoTools {
		s.denyMemberField(w, r, "runs.seed_auto_tools", "governance_profile", fmt.Sprintf(
			"`seed_auto_tools` is not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"and the pre-attach seed runs before any human is at the pane. Launch without it.",
			name, level, bound))
		return res, nil, false
	}
	// requestIsInteractive, never req.Interactive: a request with no task
	// coerces to interactive inside decodeAndValidateCreateRun, and preflight
	// never runs that coercion at all — so the raw field would refuse runs that
	// are in fact interactive, and would refuse them on one door only.
	interactive := requestIsInteractive(*req)
	if level.Rank() < types.AutonomyL1.Rank() && !interactive {
		s.denyMemberField(w, r, "runs.interactive", "governance_profile", fmt.Sprintf(
			"unattended runs are not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"which requires a human at the pane. Launch with `--interactive`, or narrow the run's egress, secrets or confinement.",
			name, level, bound))
		return res, nil, false
	}
	warnings, ok := s.autonomyDerive(w, r, req, level, bound, name, interactive)
	return res, warnings, ok
}

// autonomyDerive is the gate's L1 half — the rung that PERMITS an unattended
// run but not an unsupervised one — plus the one warning that is true at every
// rung. Split from the refusals above so each function holds one decision (and
// so resolveRunAutonomy stays under the complexity gate).
//
// Returns ok=false once it has written its 403, and otherwise the 201
// warnings. `bound` arrives already rendered (autonomyBoundList) so this half
// and the refusals above name the same causes by construction.
func (s *Server) autonomyDerive(w http.ResponseWriter, r *http.Request, req *createRunRequest,
	level types.AutonomyLevel, bound, name string, interactive bool,
) ([]string, bool) {
	var warnings []string
	// One honest sentence, whatever the rung: an agent with no hold lane has no
	// in-sandbox half at all, so the level is enforced at this door and at the
	// proxy and by nothing inside the container. Skipped for an exec run, which
	// has no agent process to say it about (and may legitimately name no agent).
	if req.TaskMode != "exec" && !agentHasHoldLane(req.Agent) {
		warnings = append(warnings, fmt.Sprintf(
			"%s has no Wardyn tool-approval lane: autonomy level %s is enforced at launch and by the proxy, and nothing inside the sandbox gates this agent's tool calls",
			autonomyAgentLabel(req.Agent), level))
	}
	// Derivation is NON-INTERACTIVE ONLY, for the structural reason
	// effectiveToolApprovals states: applyDispatchModeEnv writes
	// WARDYN_TOOL_APPROVALS when !interactive alone, and an interactive run
	// REFUSES an explicit hold (interactiveToolApprovalsError), so deriving one
	// on that lane would answer 201 while delivering none of the supervision.
	if level != types.AutonomyL1 || interactive {
		return warnings, true
	}
	// Scoped to exactly the case where the hold WOULD be derived: an agent with
	// no external tool-approval contract cannot honour it, so a derived hold
	// there ships the unsupervised run the explicit-hold 400 already rejects —
	// the same contradiction, arriving through a field the caller never set.
	if !agentHasHoldLane(req.Agent) {
		s.denyMemberField(w, r, "runs.agent", "governance_profile", fmt.Sprintf(
			"%s is not supported under your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), which routes an "+
				"unattended run's tool calls to a Wardyn approval, and this agent has no external tool-approval contract. Launch a different agent, or launch interactively.",
			autonomyAgentLabel(req.Agent), name, level, bound))
		return nil, false
	}
	if req.ToolApprovals == "hold" {
		return warnings, true // already what the caller asked for: nothing was derived, so there is nothing to report
	}
	req.ToolApprovals = "hold"
	return append(warnings, fmt.Sprintf(
		"tool_approvals was set to hold: your governance profile %q permits autonomy level %s for this run's posture (bound by %s), "+
			"so this run's tool calls wait for your approval instead of running unsupervised",
		name, level, bound)), true
}

// autonomyBoundList renders the tied causes into the clause every refusal and
// the derived-hold warning carry: "egress_open", "egress_open and
// secrets_powerful", "egress_open, secrets_powerful and confinement_cc1".
//
// EVERY cause, never the first. A member reads this sentence to learn what to
// narrow and an admin reads it to learn which rubric row to edit, and with a
// tie the first cause is not the answer: raise egress_open alone and the level
// does not move. The order is FoldAutonomy's fixed field order, so the same
// run reads the same sentence on Review and at launch.
//
// The empty case is UNREACHABLE — resolveRunAutonomy returns before the ladder
// when nothing bound the level — and degrades to a readable phrase rather than
// to an empty parenthetical.
func autonomyBoundList(boundBy []string) string {
	switch len(boundBy) {
	case 0:
		return "its rubric"
	case 1:
		return boundBy[0]
	}
	return strings.Join(boundBy[:len(boundBy)-1], ", ") + " and " + boundBy[len(boundBy)-1]
}

// agentHasHoldLane reports whether an agent can actually honour
// tool_approvals=hold — whether the image's launcher has the agent-side half
// (agent-run's hold branch runs claude under `--permission-mode manual` with
// wardyn-toolgate as the permission prompt tool; see applyDispatchModeEnv,
// runs_dispatch_mounts.go).
//
// An ALLOWLIST, not a codex-cli denylist, and the direction is the whole
// safety of it: a BYOA image or a WARDYN_AGENT_IMAGES-only custom agent has no
// Wardyn launcher at all, so a denylist would derive a hold into a sandbox
// that ignores it — the "field accepted and thrown away" failure
// interactiveToolApprovalsError exists to refuse. The hard-coded 400 in
// decodeAndValidateCreateRun spells the same fact for the one agent a caller
// can name explicitly today.
func agentHasHoldLane(agent string) bool {
	return agent == "claude-code"
}

// autonomyAgentLabel names an agent in a refusal or a warning. An empty agent
// is a BYOA run — an image and no harness — and "" reads as a missing word.
func autonomyAgentLabel(agent string) string {
	if agent == "" {
		return "this run's image"
	}
	return "agent " + agent
}

// autonomyPostureSpec returns the spec the posture is graded on: the FOLDED
// spec widened by every egress lane unionRunEgress adds that BOTH doors can
// compute.
//
// The widening is what makes the two doors agree, and it is not optional.
// Launch's unionRunEgress runs AFTER the create audit row is written, so
// launch's spec at the gate is pre-union; preflight, which has no run id,
// unions the workspace lanes before it folds anything. Grading each door's
// spec as it stands would therefore have an enterprise-forge host read
// "sealed" at launch and "open" on Review. Unioning here instead is a no-op on
// preflight's spec for the lanes it already ran and set-identical on launch's,
// so both doors grade the same envelope whatever order the unions ran in.
//
// Three lanes, and the third is the one a narrower reading of "pre-union"
// would have left out at real cost. The site-config SCM hosts are NOT
// grant-dependent: unionRunEgress gates them on `declaresRepo`, which is true
// from `spec.WorkspaceRepos` or the legacy free-text `repo` field alone
// (runs_create.go), and unionSiteConfigScmHosts reads nothing but site config.
// So `--repo https://ghes.corp.example/team/app` reaches an operator-declared
// internal forge with no grant, no workspace and no approval — and a posture
// blind to it grades that run `sealed`, handing it the rubric's most
// permissive egress rung. The same Server method launch calls is called here,
// so the two cannot drift about which hosts those are.
//
// KNOWN GAP, named in the changelog: the two GRANT-DERIVED lanes — an
// ssh_key's SSH-over-443 endpoint and a git_pat's Azure DevOps bundle — are
// still outside the posture, along with the grant-only path into
// `declaresRepo`. Those hosts come out of persistRunGrants, past a per-host
// provider-lane veto that runs only on the launch side, and re-deriving that
// decision here is how the two doors start disagreeing again. The residual is
// BOUNDED rather than merely accepted: every grant that opens one of those
// lanes is an ssh_key or a git_pat, which autonomySecrets grades `powerful` at
// both doors, so such a run is never graded as carrying nothing.
//
// Works on a copy with both domain slices cloned: spec is the one the caller
// goes on to persist and dispatch, and unionDomains appends in place.
func (s *Server) autonomyPostureSpec(ctx context.Context, spec types.RunPolicySpec,
	wsRefs []types.Workspace, legacyRepo string,
) types.RunPolicySpec {
	out := spec
	out.AllowedDomains = slices.Clone(spec.AllowedDomains)
	out.DeniedDomains = slices.Clone(spec.DeniedDomains)
	unionWorkspaceEgress(&out, wsRefs)
	for _, ws := range wsRefs {
		unionAllowedDomains(&out, workspaceCloneEgress(ws))
	}
	// unionRunEgress's declaresRepo, restricted to the two disjuncts that need
	// no grant. A run that declares no repo at all inherits no SCM lane there
	// and must inherit none here either, or a sealed local-dir run would grade
	// open on hosts it can never reach.
	if len(spec.WorkspaceRepos) > 0 || strings.TrimSpace(legacyRepo) != "" {
		s.unionSiteConfigScmHosts(ctx, &out)
	}
	return out
}
