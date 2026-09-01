// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reservedRunTasks are run.Task discriminators the SERVER sets, on their own
// separately-authorized launch paths, to switch on privileged downstream
// behavior — never legitimate input on the general POST /runs door. Rejected
// wholesale in decodeAndValidateCreateRun (W15-d) so "server-side only" is
// enforced once, for every consumer, rather than trusted per-callsite.
//
//   - harnessLoginTask (harnesscred.go): the sharpest case — its consumers
//     (handleUploadSSOToken, runIsUnrecordable/attach.go) trust run.Task ALONE,
//     with no second trusted-linkage field to fall back on, so this is the
//     only defense for them.
//   - "workspace record" (workspace_run.go/record.go): gates record + confined-
//     verify upload/reconcile. Belt-and-suspenders here — those consumers are
//     additionally gated on run.WorkspaceID, a column an ordinary create-run
//     request can never set (seedRequestWorkspace's own doc).
//   - "workspace verify": the retired verify-pipeline's discriminator
//     (superseded by "workspace record" + confined=true, kept live only in
//     test fixtures) — blocked defensively in case anything still recognizes it.
var reservedRunTasks = map[string]bool{
	harnessLoginTask:   true,
	"workspace record": true,
	"workspace verify": true,
}

// Length ceilings for the run's free-text display fields. Both are trust
// boundaries: the values land in a TEXT column and are rendered on every run
// row in the console. Generous enough that no real name or note is refused.
const (
	maxRunTitleLen       = 200
	maxRunDescriptionLen = 2000
)

// decodeAndValidateCreateRun decodes the POST /api/v1/runs body and applies the
// fail-closed request-shape checks: agent required, BYOI/devcontainer
// exclusivity, a known confinement_class, the task_mode enum, and (the one
// check that DOES need the store) integration_id naming a real AI-provider
// integration. On any
// violation it writes the HTTP error itself and returns ok=false. Extracted
// verbatim from handleCreateRun.
//
// It also returns an advisory warning (empty when none): a non-interactive
// request with no task would dispatch a sandbox that execs nothing and never
// reaches a terminal state on its own (task_mode-independent — exec mode also
// needs a command), so it is coerced to interactive here, at the one
// chokepoint every caller (CLI, SDK, raw API) routes through, rather than
// silently launching a run that just sits there unexplained.
//
// It also returns the caller's RESOLVED governance ceiling (zero-valued for an
// operator, who short-circuits before any store read). handleCreateRun needs it
// twice more after this returns — for the workspace-egress warning and for the
// dispatch-time deny re-assertion — and one resolution per create is the point:
// the rows are read live, so a second resolve could hand a run a different
// ceiling than the one its own refusals were decided under.
func (s *Server) decodeAndValidateCreateRun(w http.ResponseWriter, r *http.Request) (createRunRequest, governanceCeiling, types.ConfinementClass, string, bool) {
	var req createRunRequest
	var noCeiling governanceCeiling
	if !decodeStrict(w, r, &req) {
		return req, noCeiling, "", "", false
	}
	// Only agent is hard-required. Repo is OPTIONAL: an inline-policy run that
	// mounts a local host folder (WorkspaceMount target /work) has no git repo to
	// clone, so requiring a repo would block the local-folder wizard path. The
	// clone wiring in dispatch is already nil/empty-safe (it surfaces no repo env
	// when run.Repo is blank), so an empty repo simply runs in the mounted
	// workspace (or an empty one).
	if msg := agentRequirementError(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return req, noCeiling, "", "", false
	}

	// W15-d (CRIT, rbac-bypass): reject a client-supplied task that forges a
	// server-set discriminator (reservedRunTasks below) — e.g. a plain member
	// POSTing task="harness login" used to reach every consumer that trusts
	// run.Task == harnessLoginTask alone (handleUploadSSOToken lets the run
	// write the reserved AWS-SSO credential; runIsUnrecordable drops attach
	// recording), completely bypassing the operatorOnly gate on the real
	// /setup/harness-login door. One guard at this single chokepoint — every
	// POST /runs caller passes through decodeAndValidateCreateRun — makes
	// harnesscred.go's "set SERVER-SIDE, never from client input" doc true for
	// every downstream consumer at once, rather than relying on each one to
	// independently defend itself.
	if reservedRunTasks[req.Task] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q is set by the server and cannot be requested directly", req.Task))
		return req, noCeiling, "", "", false
	}

	// Item 5 + HIGH-3 review fix, plus the 0.6 image/workspace capabilities:
	// the request-level fields a member does not get to choose freely (see
	// denyMemberRequest) — checked before the XOR/builder validation below so a
	// member's request is refused with 403, never a 400 that implies the shape
	// alone is the problem.
	//
	// It hands back the caller's RESOLVED governance ceiling so the tool-approval
	// derivation at the tail of this function does not have to resolve a second
	// one: two resolves in one request could disagree (the rows are read live),
	// and a request refused under one ceiling must never be derived under
	// another. Zero-valued for an operator, who short-circuits before the read.
	ceiling, denied := s.denyMemberRequest(w, r, req)
	if denied {
		return req, noCeiling, "", "", false
	}

	// BYOI validation (fail closed before any store write): a user-supplied image
	// is mutually exclusive with a devcontainer build, and — unlike DevcontainerRepo,
	// which degrades to the convention image — an explicitly chosen image with no
	// ImageBuilder wired is a hard error (never silently swap a chosen image for the
	// convention one). seedRequestWorkspace's caller re-runs this SAME check after
	// workspace_id is resolved — a workspace's base_image can ALSO set req.Image,
	// after this ran, and must clear the identical gate (see validateImageBuildRequest).
	if msg := s.validateImageBuildRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return req, noCeiling, "", "", false
	}

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return req, noCeiling, "", "", false
	}

	// task_mode is a tiny closed enum; reject anything else up front (fail
	// closed, same shape as confinement_class above).
	if req.TaskMode != "" && req.TaskMode != "harness" && req.TaskMode != "exec" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown task_mode %q (want harness or exec)", req.TaskMode))
		return req, noCeiling, "", "", false
	}

	// interactive_start is task_mode's interactive counterpart and gets the same
	// closed-enum treatment. NOTE there is deliberately no title requirement
	// here: the console requires one, but the site-config probe, harness login
	// and workspace record/verify all create runs with no human to name them,
	// so a 400 would break every one of them. Title is a display field; the
	// console is where it is required.
	if req.InteractiveStart != "" && req.InteractiveStart != "shell" && req.InteractiveStart != "agent" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown interactive_start %q (want shell or agent)", req.InteractiveStart))
		return req, noCeiling, "", "", false
	}

	// tool_approvals gates whether an AUTONOMOUS (non-interactive) Claude Code
	// run's own tool calls route to a Wardyn approval instead of running
	// unsupervised — same closed-enum treatment as task_mode/interactive_start
	// above.
	if req.ToolApprovals != "" && req.ToolApprovals != "auto" && req.ToolApprovals != "hold" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown tool_approvals %q (want auto or hold)", req.ToolApprovals))
		return req, noCeiling, "", "", false
	}
	// codex-cli has no external tool-approval contract (Part C's spike verified
	// that only as far as codex's own docs go) — refuse the request outright
	// rather than silently falling back to today's unsupervised skip-permissions,
	// which would contradict the "hold" the caller explicitly asked for.
	if req.ToolApprovals == "hold" && req.Agent == "codex-cli" {
		writeError(w, http.StatusBadRequest, "tool_approvals=hold is not supported for codex-cli (no external tool-approval contract)")
		return req, noCeiling, "", "", false
	}

	// Title/description are free text bound for a TEXT column and every run row
	// in the console — cap them at the door rather than discovering a 40KB
	// "title" in the list view. Generous enough that no real name is refused.
	// Runes, not bytes: the message says "chars", and a CJK/emoji title well
	// under the limit was refused with a byte count the operator couldn't
	// reconcile with what they typed.
	if utf8.RuneCountInString(req.Title) > maxRunTitleLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("title is too long (%d chars, max %d)", utf8.RuneCountInString(req.Title), maxRunTitleLen))
		return req, noCeiling, "", "", false
	}
	if utf8.RuneCountInString(req.Description) > maxRunDescriptionLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("description is too long (%d chars, max %d)", utf8.RuneCountInString(req.Description), maxRunDescriptionLen))
		return req, noCeiling, "", "", false
	}

	// A run-explicit integration_id must name a real, run-selectable
	// (AI-provider) integration — checked eagerly, before any run is created,
	// so a typo or a source-control / corporate-network id (operator-wide,
	// never run-selectable) fails loud here rather than silently resolving to
	// nothing at foldRunIntegration time (llmcred.go).
	if req.IntegrationID != "" {
		if in, ok := s.resolveIntegrationRef(r.Context(), s.secretOwnerFromRequest(r), req.IntegrationID); !ok || !types.AIProviderKind(in.Kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("integration_id %q does not name an AI provider integration", req.IntegrationID))
			return req, noCeiling, "", "", false
		}
	}

	// W15-S1-2: a non-interactive request with no task would dispatch a sandbox
	// that execs nothing and never reaches a terminal state on its own —
	// coerce to interactive (idle, attachable, reapable) instead of silently
	// launching a run that just sits there unexplained.
	var warning string
	if !req.Interactive && strings.TrimSpace(req.Task) == "" {
		req.Interactive = true
		warning = "no task and not --interactive: the sandbox comes up idle instead of running nothing forever; attach with `wardyn attach <run-id>` or pass a task"
	}
	if msg := interactiveToolApprovalsError(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return req, noCeiling, "", "", false
	}
	// LAST, after the coercion above and after every refusal: a governance
	// profile whose tool_rules hold or deny puts this run in the hold lane even
	// though the caller asked for nothing (or asked for `auto`). Sited here so
	// the two shapes a derived hold could contradict are already settled — the
	// codex-cli refusal and the interactive one, both raised above with the
	// profile's own wording (denyMemberRequest) rather than the generic 400s a
	// derived value would otherwise trip.
	req.ToolApprovals = effectiveToolApprovals(req, ceiling)
	return req, ceiling, reqCC, warning, true
}

// requestIsInteractive reports a create request's POST-COERCION interactivity:
// a request with no task becomes interactive at the coercion above, so the raw
// req.Interactive is the wrong question everywhere the ANSWER matters.
//
// It exists because two callers need that answer BEFORE the coercion runs:
// denyMemberRequest (which is called at :97, and whose deny_interactive limit is
// otherwise evaded by simply omitting the task) and effectiveToolApprovals. The
// expression is deliberately the same one the coercion itself branches on — one
// definition, so the gate and the coercion cannot disagree about what a
// task-less request is.
func requestIsInteractive(req createRunRequest) bool {
	return req.Interactive || strings.TrimSpace(req.Task) == ""
}

// governanceHoldRules reports whether an ASSIGNED profile's ceiling demands
// supervision — any resolved tool rule that holds or denies.
//
// SCOPED TO AN ASSIGNED PROFILE, and that is the whole safety of the mechanism
// (§A, PF-27). `tool_rules` already lives in Config.DefaultPolicy on real
// deployments, where it is consulted only when a request explicitly asks for
// `hold`; keying derivation on the RULES rather than on the assignment would
// flip every run on such a deployment into the hold lane and fire the codex
// refusal deployment-wide, on an upgrade that changed no configuration. Absent
// row, absent behaviour change.
func governanceHoldRules(ceiling governanceCeiling) bool {
	if ceiling.Profile == nil {
		return false
	}
	for _, r := range ceiling.Spec.ToolRules {
		if r.Effect == types.ToolHold || r.Effect == types.ToolDeny {
			return true
		}
	}
	return false
}

// effectiveToolApprovals is the autonomy derivation: under a profile whose
// tool_rules hold or deny, a NON-INTERACTIVE run routes its tool calls through
// the gate whether or not the caller asked.
//
// It overrides an explicit `auto`, deliberately — that is the escape it closes.
// A member under a supervision-demanding profile who could opt out by sending
// one field would make the profile advisory, and `tool_approvals` is the only
// switch between "every gated call is answered" and "none are".
//
// NON-INTERACTIVE ONLY (PF-27). An interactive run's tool use is already
// supervised by the human at the attach pane — that is exactly why
// interactiveToolApprovalsError REFUSES an explicit hold there — and dispatch
// writes WARDYN_TOOL_APPROVALS for non-interactive runs alone, so deriving on
// the interactive lane would brick the default console flow while adding no
// supervision at all. A profile that wants that lane closed uses
// limits.deny_interactive, the lever built for it.
func effectiveToolApprovals(req createRunRequest, ceiling governanceCeiling) string {
	if requestIsInteractive(req) || !governanceHoldRules(ceiling) {
		return req.ToolApprovals
	}
	return "hold"
}

// agentRequirementError reports why a request may not omit `agent`, or "" when
// it may.
//
// D1: exec mode runs the task as a plain shell command — no agent harness, no
// model call — so naming an agent there was a formality that made the CLI read
// AI-first to someone who wanted a governed shell. docs/CI.md documented the
// workaround it forced: an exec run naming an agent it never uses, in the
// document whose entire audience is exec.
//
// NOT A BLANKET DEFAULT, for two reasons that are easy to get wrong:
//
//  1. Defaulting to "claude-code" would be a SECURITY decision, not a
//     convenience: the agent feeds the managed-subscription eligibility test, so
//     every agentless exec run would become eligible for the operator's live
//     subscription credential.
//  2. Defaulting to "byoa"/"none" resolves to an image that DOES NOT EXIST. The
//     ghcr convention fallback yields agent-byoa / agent-none, both unpublished
//     — and the catalog's BYOA row is {ID: "none"}, so even the "correct" key
//     404s. The run would 201 and then fail at pull, which an acceptance test
//     asserting 201 would happily pass.
//
// So: require an image the run can actually start from, and keep the 400
// otherwise, naming what is missing. Harness mode is unchanged — a harness run
// with no agent is a sandbox that comes up and runs no agent.
func agentRequirementError(req createRunRequest) string {
	if req.Agent != "" {
		return ""
	}
	if req.TaskMode != "exec" {
		return "agent is required"
	}
	if strings.TrimSpace(req.Image) != "" || len(req.Workspaces) > 0 {
		return ""
	}
	return "agent is required unless the run names an image to run in: pass --image, attach a workspace, or pass --agent"
}

// interactiveToolApprovalsError refuses tool_approvals=hold on an interactive
// run, which used to be accepted and silently discarded: applyDispatchModeEnv
// writes WARDYN_TOOL_APPROVALS only when !interactive, so the caller got a 201
// and none of the supervision they asked for. A field accepted and thrown away
// is worse than one refused — the caller believes the run is gated.
//
// The run does NOT become unsupervised: interactive tool use is supervised by
// default (the agent parks its own approval prompt in the pane until a human
// attaches), and the interactive lever is the inverted SeedAutoTools. So this
// refuses a contradiction rather than closing a hole.
//
// Its CALLER must run this AFTER the empty-task→interactive coercion: a request
// with no task and interactive=false becomes interactive there, so a check
// sited earlier passes and the field is still dropped — the exact hole, one
// step later.
func interactiveToolApprovalsError(req createRunRequest) string {
	if !req.Interactive || req.ToolApprovals != "hold" {
		return ""
	}
	return "tool_approvals=hold is not supported for an interactive run: it applies to autonomous runs only, " +
		"and an interactive run's tool use is already supervised in the attach pane. " +
		"Drop tool_approvals, or launch without --interactive."
}

// denyMemberRequest is the REQUEST-LEVEL half of a member's launch gate: the
// fields the inline_policy clamp (resolveRunPolicy) never touches because they
// are not policy at all. Reports true — having written the 403 and an
// authz.denied audit event — when the run must not proceed; callers must return
// immediately. An operator is exempt in one line at the top, so nothing below
// ever costs them a store read.
//
// Five fields, three different answers:
//
//   - devcontainer_repo is UNCONDITIONALLY operator-only, and is not a
//     capability kind at all. It hands attacker-authored build configuration
//     (the devcontainer.json plus whatever Dockerfile/setup steps it points at)
//     to the image builder, which is not a power to hand out one row at a time.
//   - image WIDENS: 0.5 refused every member outright, and a grant of the exact
//     ref is what makes one nameable (capGranted — a grant AND the switch, see
//     its own comment). Same build path as devcontainer_repo, but a pinned ref
//     an admin wrote down is a bounded thing; a repo whose contents change
//     under them is not.
//   - workspace, agent and integration_id all NARROW: each is something every
//     member could already do, so each stays allowed until an admin enforces
//     its kind (denyMemberCapability, capSeamAllowed). Each gates the member's
//     OWN choice and nothing else — never the workspace a stored policy or a
//     scan linkage brings in, never the workspace pin or site default
//     resolveRunIntegration falls back to, all of which are admin-authored
//     (the doctrine in OPERATIONS §Multi-user).
//
// A workspace's own base_image (seedRequestWorkspace, called AFTER this) is
// deliberately NOT gated here: it is operator-authored config (the workspace was
// onboarded through an operator-only route), never a member's own free-text
// choice, and seedRequestWorkspace only ever sets req.Image when the caller left
// it empty — so this check, run BEFORE that seeding, can never catch it.
// Workspaces have no equivalent devcontainer_repo seed at all.
//
// DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
// RoleSecurityAdmin): the security admin AUTHORS policy for others but RUNS
// under the deployer's ceiling themselves. Exempting them here — and at the
// inline-policy clamp (inline_policy.go), its lockstep twin — would let the
// principal who writes the org's ceilings be the one principal none of them
// bind, which is the self-exemption the whole tier is designed not to have.
//
// It ALSO returns the caller's resolved governance ceiling (zero-valued for the
// operator short-circuit, which never reads one): the create path's tool-
// approval derivation needs the same ceiling this function's refusals were
// decided under, and re-resolving would read the rows a second time.
func (s *Server) denyMemberRequest(w http.ResponseWriter, r *http.Request, req createRunRequest) (governanceCeiling, bool) {
	if s.isOperator(r.Context()) {
		return governanceCeiling{}, false
	}
	if req.DevcontainerRepo != "" {
		return governanceCeiling{}, s.denyMemberField(w, r, "runs.image", "byoi_member",
			"a custom devcontainer repo (devcontainer_repo) is operator-only; launch with the agent's convention image or an onboarded workspace's base image")
	}
	if req.Image != "" {
		granted, err := s.capGranted(r.Context(), capImage, req.Image)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "resolve capability: "+err.Error())
			return governanceCeiling{}, true
		}
		if !granted {
			return governanceCeiling{}, s.denyMemberField(w, r, "runs.image", "byoi_member",
				"image "+req.Image+" is not granted to you — ask an admin to grant the exact image ref, "+
					"or launch with the agent's convention image or an onboarded workspace's base image")
		}
	}
	if req.WorkspaceID != nil && s.denyMemberCapability(w, r, capWorkspace, req.WorkspaceID.String(), "runs.workspace",
		"you are not granted workspace "+req.WorkspaceID.String()+" — ask an admin for access, or launch without a workspace") {
		return governanceCeiling{}, true
	}
	// An empty agent names nothing to bound — an exec run may legitimately omit
	// it (agentRequirementError), and gating "" would refuse those on a kind
	// that has nothing to say about them.
	if req.Agent != "" && s.denyMemberCapability(w, r, capAgent, req.Agent, "runs.agent",
		"you are not granted agent "+req.Agent+" — ask an admin to grant it, or launch one you hold") {
		return governanceCeiling{}, true
	}
	// TIER 1 ONLY (PF-33, capIntegration's own comment): the run-explicit
	// integration_id is the sole member-authored tier. A workspace's pin and the
	// operator's site default fold on untouched.
	if req.IntegrationID != "" && s.denyMemberCapability(w, r, capIntegration, req.IntegrationID, "runs.integration",
		"you are not granted integration "+req.IntegrationID+" — ask an admin to grant it, or launch without integration_id") {
		return governanceCeiling{}, true
	}
	ceiling, err := s.effectiveCeiling(r.Context())
	if err != nil {
		writeCeilingError(w, err)
		return governanceCeiling{}, true
	}
	if s.denyMemberGovernance(w, r, req, ceiling) {
		return governanceCeiling{}, true
	}
	return ceiling, false
}

// denyMemberCapability is the NARROWING seam the request-level kinds share:
// resolve, 500 on a store that cannot answer, and one refusal carrying the
// kind's own `capability_<kind>` reason. Returns true when the caller must stop.
//
// One helper, not one block per kind, so the three narrowing gates cannot drift
// apart on the thing that matters — capSeamAllowed, NEVER capGranted. Every kind
// routed here is one a member could already use in 0.5, so an unenforced kind
// must stay allowed; capGranted answers the opposite (widening) question and
// refuses on !enforced, which is why capImage keeps its own call site above.
func (s *Server) denyMemberCapability(w http.ResponseWriter, r *http.Request, kind, value, target, msg string) bool {
	allowed, err := s.capSeamAllowed(r.Context(), kind, value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve capability: "+err.Error())
		return true
	}
	if allowed {
		return false
	}
	return s.denyMemberField(w, r, target, "capability_"+kind, msg)
}

// denyMemberGovernance is denyMemberRequest's governance half: the request
// SHAPES an assigned profile can refuse, plus its one quota. Split out so
// denyMemberRequest's branch count stays under the gocyclo gate, exactly as
// clampOperatorSwitches is split out of Clamp.
//
// Every one keys on `ceiling.Profile != nil`, never on the ceiling's contents —
// an UNASSIGNED member is byte-for-byte today here (PF-1's stated residual), and
// the deployment default carrying hold/deny tool_rules must not refuse anything
// deployment-wide on an upgrade that changed no configuration.
//
// The strings are the mock round's frozen member copy (docs/design/
// governance-prompt.md §7.7) and are reproduced BYTE-EXACT: the console never
// rewords a server refusal, so this file is where that copy actually ships.
func (s *Server) denyMemberGovernance(w http.ResponseWriter, r *http.Request, req createRunRequest, ceiling governanceCeiling) bool {
	if ceiling.Profile == nil {
		return false
	}
	name := ceiling.Profile.Name
	// exec runs a bare command: no agent, no toolgate, nothing for tool_rules to
	// bind. A profile that wants supervised tool use has to be able to close the
	// door that routes around the gate entirely.
	if ceiling.Limits.DenyTaskModeExec && req.TaskMode == "exec" {
		return s.denyMemberField(w, r, "runs.task_mode", "governance_profile", fmt.Sprintf(
			"`task_mode=exec` is not allowed by your governance profile %q — an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead.", name))
	}
	// POST-COERCION, and that is the whole gate. req.Interactive is still the RAW
	// field here — this function runs before the empty-task→interactive coercion
	// — so reading it directly would be evaded by simply omitting the task, which
	// is the one request shape a deny_interactive profile most needs to refuse.
	if ceiling.Limits.DenyInteractive && requestIsInteractive(req) {
		return s.denyMemberField(w, r, "runs.interactive", "governance_profile", fmt.Sprintf(
			"interactive runs are not allowed by your governance profile %q, and a request with no task comes up interactive too. Launch with a task, and without `--interactive`.", name))
	}
	if s.denyMemberRunQuota(w, r, ceiling) {
		return true
	}
	if !governanceHoldRules(ceiling) {
		return false
	}
	// PF-31. The pre-attach seed span runs skip-permissions with no toolgate and
	// no human at the pane yet, so PF-27's "interactive is human-supervised"
	// rationale is explicitly false for it — which is why this refusal is NOT
	// scoped to the derivation's non-interactive lane the way the codex one is.
	if req.SeedAutoTools {
		return s.denyMemberField(w, r, "runs.seed_auto_tools", "governance_profile", fmt.Sprintf(
			"`seed_auto_tools` is not allowed by your governance profile %q: its tool rules hold or deny, and the pre-attach seed runs before any human is at the pane. Launch without it.", name))
	}
	// PF-18. Scoped to exactly the case where effectiveToolApprovals WOULD derive
	// hold: codex-cli has no external tool-approval contract, so a derived hold
	// there would silently ship the unsupervised run the explicit-hold refusal
	// above (:152) exists to reject — the same contradiction, arriving through a
	// field the caller never set.
	if req.Agent == "codex-cli" && !requestIsInteractive(req) {
		return s.denyMemberField(w, r, "runs.agent", "governance_profile", fmt.Sprintf(
			"codex-cli is not supported under your governance profile %q: its tool rules hold or deny, and codex-cli has no external tool-approval contract. Launch a different agent.", name))
	}
	return false
}

// denyMemberRunQuota is the third limit and the one that is NOT a refusal of a
// request shape: MaxConcurrentRuns caps how many non-terminal runs one assigned
// member may hold at once.
//
// 422 AND NO AUDIT, deliberately — the two shape limits above are 403s with an
// authz.denied row because the caller asked for something they may not have;
// this caller IS authorized and is simply at a quota. Both existing per-principal
// caps say it exactly this way (apiTokenMaxPerPrincipal, sshMaxKeysPerPrincipal:
// 422, "too many … — X one first", no audit event), and a quota that filled the
// denial stream with authz.denied rows would make a busy member look like an
// attacker.
//
// 0 (the zero value, and any negative) is UNLIMITED, matching the two booleans'
// rule: a profile that omits limits behaves exactly as one written before this
// field existed. ASSIGNED SUBJECTS ONLY — an unassigned member has no profile,
// so there is no cap to read and no global default one to fall back to (PF-36:
// the `all` assignment IS the opt-in for a deployment-wide cap).
//
// ponytail: count-then-create, so two simultaneous creates can both see N-1 and
// both land. Accepted, exactly as the two 20-caps above accept it — the cap is a
// runaway-loop guard, not a licence meter, and a transaction around create just
// to make a soft cap exact is not worth the write path it would complicate.
func (s *Server) denyMemberRunQuota(w http.ResponseWriter, r *http.Request, ceiling governanceCeiling) bool {
	limit := ceiling.Limits.MaxConcurrentRuns
	if limit <= 0 {
		return false
	}
	active, err := s.cfg.Store.CountActiveRunsBy(r.Context(), principalFromRequest(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count active runs: "+err.Error())
		return true
	}
	if active < limit {
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
		"too many runs at once (max %d) — your governance profile %q caps how many runs you can have going, and %d are still active. Stop one first.",
		limit, ceiling.Profile.Name, active))
	return true
}

// denyMemberSeededImage closes G3 (PF-34, live since 0.6.0): a MEMBER-OWNED
// workspace's base_image is copied into req.Image by seedRequestWorkspace AFTER
// denyMemberRequest has already run, and the follow-up re-validation
// (validateImageBuildRequest) only re-checks the XOR and the builder — never
// capGranted(capImage, …). So a member onboards a workspace whose base_image is
// any ref they like, launches against it, and reaches the one WIDENING
// capability the product has without holding a grant for it.
//
// OWNERSHIP-SCOPED, and the scoping is not a nicety. seededOwner is non-empty
// only when the seed actually set req.Image AND the seeding workspace was
// member-owned; an operator-authored workspace's base_image stays exactly what
// denyMemberRequest's own doc says it is — operator config, not a member's
// free-text choice — and this function no-ops on it, byte-for-byte today.
//
// THE TRAP, named because the next refactor will reach for it: an UNCONDITIONAL
// re-check here is a catastrophic regression, not a stricter version of this
// one. capGranted answers the WIDENING question, so it REFUSES on !enforced
// (capabilities.go) — which means an unconditional call would 403 every member
// run against every base-image workspace on every deployment that has not
// enforced capImage, i.e. all of them on upgrade day.
func (s *Server) denyMemberSeededImage(w http.ResponseWriter, r *http.Request, seededOwner, image string) bool {
	if seededOwner == "" {
		return false
	}
	granted, err := s.capGranted(r.Context(), capImage, image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve capability: "+err.Error())
		return true
	}
	if granted {
		return false
	}
	// The SAME refusal the explicit --image branch raises (target, reason and
	// shape), because it is the same capability answered about the same value —
	// only the door differs, and the message says which one.
	return s.denyMemberField(w, r, "runs.image", "byoi_member",
		"image "+image+" comes from your own workspace's base image and is not granted to you — "+
			"ask an admin to grant the exact image ref, or launch with the agent's convention image")
}

// denyMemberField writes one member refusal — the 403 and its audit row — and
// returns true so a caller can `return s.denyMemberField(...)`. One helper so a
// new gate cannot ship the error without the audit event.
func (s *Server) denyMemberField(w http.ResponseWriter, r *http.Request, target, reason, msg string) bool {
	writeError(w, http.StatusForbidden, msg)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"authz.denied", target, "denied", mustJSON(map[string]any{"reason": reason})))
	return true
}

// validateImageBuildRequest enforces the image/devcontainer_repo XOR + the
// image-builder-wired requirement. Shared by decodeAndValidateCreateRun (the
// request's own --image) and seedRequestWorkspace's caller: a workspace's
// base_image can ALSO set req.Image, AFTER decodeAndValidateCreateRun has
// already run — so the identical check must run again once that seed lands,
// or a workspace's base_image would bypass a check an explicit --image must
// pass (e.g. silently pairing with a --devcontainer-repo the user set, or
// reaching FinalizeBase with no ImageBuilder wired).
func (s *Server) validateImageBuildRequest(req createRunRequest) string {
	if req.Image == "" {
		return ""
	}
	if req.DevcontainerRepo != "" {
		return "image and devcontainer_repo are mutually exclusive"
	}
	if s.cfg.ImageBuilder == nil {
		return "a custom sandbox image was requested but this control plane has no image builder wired " +
			"(start wardynd with -tags docker and set WARDYN_ENVBUILD_TOOLS_DIR / -envbuild)"
	}
	return ""
}
