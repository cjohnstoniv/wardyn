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
// reason — the closed reason enum stays closed), or its own 500 when the site
// config could not be read. The warnings ride the 201.
//
// It also returns the site-config snapshot the SCM-host lane was graded from
// (scmLaneSiteConfig), and launch hands that same value to unionRunEgress. One
// read, two consumers: a gate that read site config and a union that read it
// again could disagree — a dropped connection or an admin's edit between the
// two would grade a run `sealed` and dispatch it to a forge. Review discards
// it, as it discards the derived field.
func (s *Server) resolveRunAutonomy(w http.ResponseWriter, r *http.Request, req *createRunRequest,
	spec types.RunPolicySpec, wsRefs []types.Workspace, enforced types.ConfinementClass,
	ceiling governanceCeiling,
) (types.AutonomyResolution, []string, types.SiteConfig, bool) {
	// Read for EVERY run that declares a repo, bound or not, because launch
	// dispatches from this snapshot whether or not a rubric graded it. Fail
	// closed, as admitRepoSources and siteConfigForLaneVeto do on the same
	// read: a posture graded on a site config nobody could read is a guess.
	scmSite, err := s.scmLaneSiteConfig(r.Context(), spec, req.Repo)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return types.AutonomyResolution{}, nil, types.SiteConfig{}, false
	}
	// No profile, or a profile with no rubric: the zero value and no bound. An
	// UNASSIGNED member — and every operator — is byte-for-byte what they were
	// before this gate existed, the same absent-row rule every other
	// GovernanceLimits field follows. Nothing below runs.
	if ceiling.Profile == nil || ceiling.Limits.AutonomyRubric == nil {
		return types.AutonomyResolution{}, nil, scmSite, true
	}
	posture := composer.AutonomyPostureOf(autonomyPostureSpec(spec, wsRefs, req.Repo, scmSite), enforced)
	level, boundBy := composer.FoldAutonomy(*ceiling.Limits.AutonomyRubric, posture)
	res := types.AutonomyResolution{Level: level, Posture: posture, BoundBy: boundBy}
	// An all-unset rubric — or one that leaves this posture's three fields
	// unset — caps nothing, identically to a nil rubric (AutonomyRubric's own
	// doc). The posture still travels, so the audit row and Review record what
	// was graded even when nothing bound it.
	if level == "" {
		return res, nil, scmSite, true
	}
	warnings, ok := s.autonomyLadder(w, r, req, level, autonomyBoundList(boundBy), ceiling.Profile.Name)
	return res, warnings, scmSite, ok
}

// autonomyLadder enforces a resolved level on the request: the refusals, then
// autonomyDerive's L1 half. Split from resolveRunAutonomy so that one owns the
// inputs (the snapshot, the posture, the fold) and this one owns the decisions.
//
// `bound` arrives rendered ONCE (autonomyBoundList), so the refusals and the
// derived-hold warning cannot drift into naming different causes for one
// resolution.
func (s *Server) autonomyLadder(w http.ResponseWriter, r *http.Request, req *createRunRequest,
	level types.AutonomyLevel, bound, name string,
) ([]string, bool) {
	// requestIsInteractive, never req.Interactive: a request with no task
	// coerces to interactive inside decodeAndValidateCreateRun, and preflight
	// never runs that coercion at all — so the raw field would refuse runs that
	// are in fact interactive, and would refuse them on one door only.
	interactive := requestIsInteractive(*req)
	// The ladder, expressed as the LOWEST level that permits each capability
	// rather than one arm per rung. Separate arms are how a hole gets shipped:
	// L0 is the most supervised rung, so anything L1 refuses it must refuse
	// too, and two independently written arms drift the moment one grows a case.
	//
	//	task_mode=exec    L3   the door that routes around every other gate
	//	shell boot seed   L3   exec's reach, arriving through an interactive run
	//	seed_auto_tools   L2   the pre-attach span runs before any human is at the pane
	//	non-interactive   L1   unattended at all
	if level.Rank() < types.AutonomyL3.Rank() && req.TaskMode == "exec" {
		s.denyMemberField(w, r, "runs.task_mode", "governance_profile", fmt.Sprintf(
			"`task_mode=exec` is not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"and an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead.",
			name, level, bound))
		return nil, false
	}
	// The shell boot seed ranks WITH exec, not with seed_auto_tools, because it
	// does what exec does: with interactive_start unset or `shell`, the image
	// runs the task as `bash -lc` at sandbox boot, as the agent user, before
	// anyone attaches — no agent, no tool approvals. Ranked any lower, a rung
	// that refuses `task_mode=exec` would hand the same command to anyone who
	// added `"interactive": true`, and the exec row above would be decoration.
	// The agent form (`claude "$seed"`) is left alone: it parks its own
	// approval prompt until a human joins, unless seed_auto_tools says otherwise.
	if level.Rank() < types.AutonomyL3.Rank() && req.InteractiveStart != "agent" && interactiveBootSeed(interactive, req.Task) != "" {
		s.denyMemberField(w, r, "runs.interactive_start", "governance_profile", fmt.Sprintf(
			"a startup command is not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"and with `interactive_start` unset or `shell` an interactive run's task runs as a shell command at sandbox boot, before anyone attaches — "+
				"what `task_mode=exec` does. Launch with `interactive_start=agent` to hand the task to the agent as its first prompt, or without a task.",
			name, level, bound))
		return nil, false
	}
	if level.Rank() < types.AutonomyL2.Rank() && req.SeedAutoTools {
		s.denyMemberField(w, r, "runs.seed_auto_tools", "governance_profile", fmt.Sprintf(
			"`seed_auto_tools` is not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"and the pre-attach seed runs before any human is at the pane. Launch without it.",
			name, level, bound))
		return nil, false
	}
	if level.Rank() < types.AutonomyL1.Rank() && !interactive {
		s.denyMemberField(w, r, "runs.interactive", "governance_profile", fmt.Sprintf(
			"unattended runs are not allowed by your governance profile %q at this run's posture: it permits autonomy level %s (bound by %s), "+
				"which requires a human at the pane. Launch with `--interactive`, or narrow the run's egress, secrets or confinement.",
			name, level, bound))
		return nil, false
	}
	return s.autonomyDerive(w, r, req, level, bound, name, interactive)
}

// autonomyDerive is the gate's L1 half — the rung that PERMITS an unattended
// run but not an unsupervised one — plus the one warning that is true at every
// rung. Split from the refusals above so each function holds one decision (and
// so autonomyLadder stays under the complexity gate).
//
// Returns ok=false once it has written its 403, and otherwise the 201
// warnings. `bound` arrives already rendered (autonomyBoundList) so this half
// and autonomyLadder's refusals name the same causes by construction.
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
// spec widened by every egress lane unionRunEgress adds.
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
// Every input below is the spec, the workspaces and the one site-config
// snapshot, so Review and launch compute it identically:
//
//   - the workspace registries and clone hosts;
//   - the site-config SCM hosts, whenever specDeclaresRepo — which counts a
//     github_token, git_pat or ssh_key grant as declaring a repo, as launch's
//     declaresRepo does. A read-only github_token alone opens this lane at
//     launch and grades only `baseline` on the secrets axis, so leaving the
//     grant-only path out handed a run that reaches an internal forge the
//     rubric's `sealed` row;
//   - a git_pat's Azure DevOps bundle and an ssh_key's SSH-over-443 endpoint,
//     from grantLaneEgress — the helper persistRunGrants itself builds those
//     lanes with, so the two cannot drift about which hosts they are.
//
// Graded BEFORE the launch-side decisions that can drop a lane — the provider
// row's per-host veto in persistRunGrants and codex-cli's missing SSH lane —
// because those exist on one door only, and re-deriving them here is how the
// two doors start disagreeing. So the allowlist graded here is a superset of
// the one unionRunEgress builds: a run whose unioned envelope is `open` is
// always graded `open`, and a run whose lane was vetoed may be graded `open`
// on reach it will not get. Lanes added later still, at dispatch, are not
// here: the model-provider hosts resolved from global configuration and the
// artifact-redirect substitution.
//
// Works on a copy with both domain slices cloned: spec is the one the caller
// goes on to persist and dispatch, and unionDomains appends in place.
func autonomyPostureSpec(spec types.RunPolicySpec, wsRefs []types.Workspace, legacyRepo string,
	scmSite types.SiteConfig,
) types.RunPolicySpec {
	out := spec
	out.AllowedDomains = slices.Clone(spec.AllowedDomains)
	out.DeniedDomains = slices.Clone(spec.DeniedDomains)
	unionWorkspaceEgress(&out, wsRefs)
	for _, ws := range wsRefs {
		unionAllowedDomains(&out, workspaceCloneEgress(ws))
	}
	// A run that declares no repo at all inherits no SCM lane at launch and
	// must inherit none here either, or a sealed local-dir run would grade
	// open on hosts it can never reach.
	if specDeclaresRepo(spec, legacyRepo) {
		unionSiteConfigScmHosts(&out, scmSite)
	}
	for _, g := range spec.EligibleGrants {
		unionAllowedDomains(&out, grantLaneEgress(g))
	}
	return out
}

// specDeclaresRepo is unionRunEgress's `declaresRepo` computed from the spec
// alone: a workspace repo, the legacy free-text `repo` field, or any grant of a
// kind that opens a git credential lane. Launch's own test reads grantWiring,
// which exists only after persistRunGrants has applied the provider-lane veto;
// this one counts the grant before the veto, so it is true whenever launch's
// is and possibly when a veto later makes launch's false.
func specDeclaresRepo(spec types.RunPolicySpec, legacyRepo string) bool {
	if len(spec.WorkspaceRepos) > 0 || strings.TrimSpace(legacyRepo) != "" {
		return true
	}
	return slices.ContainsFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
		switch g.Kind {
		case types.GrantGitHubToken, types.GrantGitPAT, types.GrantSSHKey:
			return true
		}
		return false
	})
}

// scmLaneSiteConfig is the one site-config read the SCM-host lane is decided
// from, per request (see resolveRunAutonomy for why one). Skipped — a zero
// value, no store round trip — when the spec declares no repo, since neither
// the posture nor unionRunEgress unions the lane then; specDeclaresRepo is a
// superset of launch's test, so every run launch unions the lane for was read.
func (s *Server) scmLaneSiteConfig(ctx context.Context, spec types.RunPolicySpec, legacyRepo string) (types.SiteConfig, error) {
	if s.cfg.Store == nil || !specDeclaresRepo(spec, legacyRepo) {
		return types.SiteConfig{}, nil
	}
	return s.cfg.Store.GetSiteConfig(ctx)
}
