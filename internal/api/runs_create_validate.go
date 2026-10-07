// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reservedRunTasks are run.Task discriminators the SERVER sets, on their own
// separately-authorized launch paths, to switch on privileged downstream
// behavior — never legitimate input on the general POST /runs door. Rejected
// wholesale in decodeAndValidateCreateRun so "server-side only" is
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
	if !s.decodeRunRequest(w, r, &req) {
		return req, noCeiling, "", "", false
	}
	canonicalizeRunRepos(&req, s.adoHostsLoader(r.Context()))
	// Only agent is hard-required. Repo is OPTIONAL: an inline-policy run that
	// mounts a local host folder (WorkspaceMount target /work) has no git repo to
	// clone, so requiring a repo would block the local-folder wizard path. The
	// clone wiring in dispatch is already nil/empty-safe (it surfaces no repo env
	// when run.Repo is blank), so an empty repo simply runs in the mounted
	// workspace (or an empty one).
	if msg := agentRequirementError(req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAgentRequired, msg)
		return req, noCeiling, "", "", false
	}
	// …and, once an AgentProviders block exists, that the agent NAMED is one this
	// deployment actually offers (agent_providers.go). The sibling of the check
	// above, and placed with it: "agent is required" and "and it must be an
	// enabled one" are one question the caller asks once.
	//
	// 422, not 400: the request is well formed — it names an agent that is simply
	// not offered here — and 422 is what every other org-policy refusal on this
	// path answers. Operators and members alike, because the roster is the ORG's
	// statement of what this install runs, not a per-principal ceiling. With NO
	// block nothing is refused: agentRosterRefusal short-circuits to "" and this
	// path is byte-for-byte unchanged.
	if msg, err := s.agentRosterRefusal(r.Context(), req.Agent); err != nil {
		writeServerError(w, r, "get site config", err)
		return req, noCeiling, "", "", false
	} else if msg != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonAgentNotEnabled, msg)
		return req, noCeiling, "", "", false
	}

	// Reject a client-supplied task that forges a
	// server-set discriminator (reservedRunTasks below) — e.g. a plain member
	// POSTing task="harness login" used to reach every consumer that trusts
	// run.Task == harnessLoginTask alone (handleUploadSSOToken lets the run
	// write the reserved AWS-SSO credential; runIsUnrecordable drops attach
	// recording), completely bypassing the gates on the real sign-in door
	// (POST /model-providers/{id}/sign-in). One guard at this single chokepoint — every
	// POST /runs caller passes through decodeAndValidateCreateRun — makes
	// harnesscred.go's "set SERVER-SIDE, never from client input" doc true for
	// every downstream consumer at once, rather than relying on each one to
	// independently defend itself.
	if reservedRunTasks[req.Task] {
		writeErrorReason(w, http.StatusBadRequest, reasonRunTaskReserved, fmt.Sprintf("task %q is set by the server and cannot be requested directly", req.Task))
		return req, noCeiling, "", "", false
	}

	// The request-level fields a member does not get to choose freely (see
	// denyUserRequest) — checked before the XOR/builder validation below so a
	// member's request is refused with 403, never a 400 that implies the shape
	// alone is the problem.
	//
	// It hands back the caller's RESOLVED governance ceiling so the tool-approval
	// derivation at the tail of this function does not have to resolve a second
	// one: two resolves in one request could disagree (the rows are read live),
	// and a request refused under one ceiling must never be derived under
	// another. Zero-valued for an operator, who short-circuits before the read.
	ceiling, denied := s.denyUserRequest(w, r, req)
	if denied {
		return req, noCeiling, "", "", false
	}

	// The LEGACY single `repo` field, which reaches neither denyUserRequest
	// (it is not a capability kind of its own) nor validateWorkspaceSources (it
	// is not a spec entry) — and is nonetheless cloned by the sandbox,
	// broker-minted for and unioned into the run's egress. Its provider is the
	// member's own free-text choice, so it is gated exactly like the resolved
	// spec's repos, at the chokepoint the admission verdict shares.
	//
	// req.devcontainer_repo rides the SAME call, and its siting is the point:
	// the image builder clones it SERVER-SIDE (resolveWorkspaceImage ->
	// envbuilder's ENVBUILDER_GIT_URL), and the only principals who can set it at
	// all are operators — members are refused it by denyUserRequest above. So
	// the check belongs HERE, after that return and UNCONDITIONAL on req.Image,
	// not beside the member refusal (unreachable for the field's only callers) and
	// not in validateImageBuildRequest (gated on req.Image != "").
	// One call for both provider questions over both fields (see
	// requestRepoProviderRefusals): this function is at the gocyclo ratchet, and a
	// second branch here is what tipped it over.
	if s.requestRepoProviderRefusals(w, r, req, true) { // true: this is launch, #386's gate applies
		return req, noCeiling, "", "", false
	}

	// BYOI validation (fail closed before any store write): a user-supplied image
	// is mutually exclusive with a devcontainer build, and — unlike DevcontainerRepo,
	// which degrades to the convention image — an explicitly chosen image with no
	// ImageBuilder wired is a hard error (never silently swap a chosen image for the
	// convention one). seedRequestWorkspace's caller re-runs this SAME check after
	// workspace_id is resolved — a workspace's base_image can ALSO set req.Image,
	// after this ran, and must clear the identical gate (see validateImageBuildRequest).
	if msg, reason := s.validateImageBuildRequest(req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reason, msg)
		return req, noCeiling, "", "", false
	}

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, refusal := runRequestEnums(req)
	if refusal.write(s, w, r) {
		return req, noCeiling, "", "", false
	}

	// EVERY caller-supplied free-text field, in ONE loop: a rune cap and a
	// control-character check (runs_create_fields.go). title/description have
	// been capped since they were added; repo, devcontainer_repo, task and agent
	// were bounded only by the 1 MiB body, so a megabyte "repo" reached the run
	// row, every list payload and the hash-chained audit row — and a NUL in a
	// title was a Postgres 500 instead of a 400 naming the field the caller can
	// fix. Preflight calls the SAME helper, so Review cannot preview a
	// request launch would refuse.
	if !s.validateRunTextFields(w, req) {
		return req, noCeiling, "", "", false
	}

	// No integration credentials a run's model any more — its model provider
	// does — so naming one is refused, not ignored.
	if req.IntegrationID != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonIntegrationIDRetired, mpRunNoIntegration)
		return req, noCeiling, "", "", false
	}

	// A non-interactive request with no task would dispatch a sandbox
	// that execs nothing and never reaches a terminal state on its own —
	// coerce to interactive (idle, attachable, reapable) instead of silently
	// launching a run that just sits there unexplained.
	var warning string
	if !req.Interactive && strings.TrimSpace(req.Task) == "" {
		req.Interactive = true
		warning = "no task and not --interactive: the sandbox comes up idle instead of running nothing forever; attach with `wardyn run attach <run-id>` or pass a task"
	}
	if msg := interactiveToolApprovalsError(req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonToolApprovalsHoldInteractiveConflict, msg)
		return req, noCeiling, "", "", false
	}
	// Last, after the coercion above and after every refusal: a governance
	// profile whose tool_rules hold or deny puts this run in the hold lane even
	// though the caller asked for nothing (or asked for `auto`). Sited here so
	// the two shapes a derived hold could contradict are already settled — the
	// codex-cli refusal and the interactive one, both raised above with the
	// profile's own wording (denyUserRequest) rather than the generic 400s a
	// derived value would otherwise trip.
	req.ToolApprovals = effectiveToolApprovals(req, ceiling)
	return req, ceiling, reqCC, warning, true
}

// requestIsInteractive reports a create request's POST-COERCION interactivity:
// a request with no task becomes interactive at the coercion above, so the raw
// req.Interactive is the wrong question everywhere the ANSWER matters.
//
// It exists because two callers need that answer BEFORE the coercion runs:
// denyUserRequest (called from decodeAndValidateCreateRun above, and whose
// deny_interactive limit is otherwise evaded by simply omitting the task) and
// effectiveToolApprovals. The
// expression is deliberately the same one the coercion itself branches on — one
// definition, so the gate and the coercion cannot disagree about what a
// task-less request is.
func requestIsInteractive(req createRunRequest) bool {
	return req.Interactive || strings.TrimSpace(req.Task) == ""
}

// governanceHoldRules reports whether an ASSIGNED profile's ceiling demands
// supervision — any resolved tool rule that holds or denies.
//
// Scoped to an assigned profile, and that is the whole safety of the mechanism.
// `tool_rules` already lives in Config.DefaultPolicy on real
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
// Non-interactive only. An interactive run's tool use is already
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
// Exec mode runs the task as a plain shell command — no agent harness, no
// model call — so naming an agent there was a formality that made the CLI read
// AI-first to someone who wanted a governed shell. docs/CI.md documented the
// workaround it forced: an exec run naming an agent it never uses, in the
// document whose entire audience is exec.
//
// Not a blanket default, for two reasons that are easy to get wrong:
//
//  1. Defaulting to "claude-code" would be a SECURITY decision, not a
//     convenience: the agent feeds the managed-subscription eligibility test, so
//     every agentless exec run would become eligible for the operator's live
//     subscription credential.
//  2. Defaulting to "byoa"/"none" resolves to an image that does not exist. The
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
	// The singular workspace_id door: its base_image isn't resolved until
	// seedRequestWorkspace runs (it needs a store read), which happens well
	// after this check. Admit it here and defer to that later validation —
	// "workspace <id> has no base image to run a command in" — rather than
	// refuse a workspace-backed command before its image was ever looked at.
	if req.WorkspaceID != nil {
		return ""
	}
	return "agent is required unless the run names an image to run in: pass --image, attach a workspace, or pass --agent"
}

// interactiveToolApprovalsError refuses tool_approvals=hold on an interactive
// run: applyDispatchModeEnv
// writes WARDYN_TOOL_APPROVALS only when !interactive, so accepting the field
// here would answer 201 while delivering none of the supervision the caller
// asked for. A field accepted and thrown away
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

// denyUserRequest is the REQUEST-LEVEL half of a member's launch gate: the
// fields the inline_policy clamp (resolveRunPolicy) never touches because they
// are not policy at all. Reports true — having written the 403 and an
// authz.denied audit event — when the run must not proceed; callers must return
// immediately. An operator is exempt in one line at the top, so nothing below
// ever costs them a store read.
//
// Six fields, three different answers:
//
//   - devcontainer_repo is UNCONDITIONALLY operator-only, and is not a
//     capability kind at all. It hands attacker-authored build configuration
//     (the devcontainer.json plus whatever Dockerfile/setup steps it points at)
//     to the image builder, which is not a power to hand out one row at a time.
//   - image WIDENS: it is refused to every member by default, and a grant of the exact
//     ref is what makes one nameable (capGranted — a grant AND the switch, see
//     its own comment). Same build path as devcontainer_repo, but a pinned ref
//     an admin wrote down is a bounded thing; a repo whose contents change
//     under them is not.
//   - workspace, agent and policy_id all NARROW: each is something every
//     member could already do, so each stays allowed until an admin enforces
//     its kind (denyUserCapability, capSeamAllowed). Each gates the member's
//     OWN choice and nothing else — never the workspace a stored policy or a
//     scan linkage brings in, which is admin-authored (the doctrine in
//     OPERATIONS §Multi-user).
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
// It ALSO returns the caller's resolved governance ceiling (zero-valued but for
// Operator on the operator short-circuit, which reads no rows): the create
// path's tool-approval derivation needs the same ceiling this function's
// refusals were decided under, and re-resolving would read the rows a second
// time.
func (s *Server) denyUserRequest(w http.ResponseWriter, r *http.Request, req createRunRequest) (governanceCeiling, bool) {
	ceiling, refusal := s.runRequestGovernance(r, req)
	if refusal.write(s, w, r) {
		return governanceCeiling{}, true
	}
	return ceiling, s.denyUserGovernance(w, r, req, ceiling)
}

// denyUserCapability is the NARROWING seam the request-level kinds share:
// resolve, 500 on a store that cannot answer, and one refusal carrying the
// kind's own `capability_<kind>` reason. Returns true when the caller must stop.
//
// One helper, not one block per kind, so the three narrowing gates cannot drift
// apart on the thing that matters — capSeamAllowed, NEVER capGranted. Every kind
// routed here is one a member could already use before enforcement, so an unenforced kind
// must stay allowed; capGranted answers the opposite (widening) question and
// refuses on !enforced, which is why capImage keeps its own call site above.
func (s *Server) denyUserCapability(w http.ResponseWriter, r *http.Request, kind, value, target, msg string) bool {
	return s.runCapabilityRefusal(r, kind, value, target, msg).write(s, w, r)
}

// denyUserGovernance is denyUserRequest's governance half: the request
// SHAPES an assigned profile can refuse, plus its one quota. Split out so
// denyUserRequest's branch count stays under the gocyclo gate, exactly as
// clampOperatorSwitches is split out of Clamp.
//
// Every one keys on `ceiling.Profile != nil`, never on the ceiling's contents —
// an UNASSIGNED member is byte-for-byte today here, and
// the deployment default carrying hold/deny tool_rules must not refuse anything
// deployment-wide on an upgrade that changed no configuration.
//
// The strings are the mock round's frozen member copy (docs/design/
// governance-prompt.md §7.7) and are reproduced BYTE-EXACT: the console never
// rewords a server refusal, so this file is where that copy actually ships.
func (s *Server) denyUserGovernance(w http.ResponseWriter, r *http.Request, req createRunRequest, ceiling governanceCeiling) bool {
	if s.runGovernancePosture(r, req, ceiling, true).write(s, w, r) {
		return true
	}
	if s.denyUserRunQuota(w, r, ceiling) {
		return true
	}
	return s.runGovernanceSupervision(r, req, ceiling).write(s, w, r)
}

// denyUserRunQuota is the third limit and the one that is NOT a refusal of a
// request shape: MaxConcurrentRuns caps how many non-terminal runs one assigned
// member may hold at once.
//
// 422 and no audit, deliberately — the two shape limits above are 403s with an
// authz.denied row because the caller asked for something they may not have;
// this caller IS authorized and is simply at a quota. Both existing per-principal
// caps say it exactly this way (apiTokenMaxPerPrincipal, sshMaxKeysPerPrincipal:
// 422, "too many … — X one first", no audit event), and a quota that filled the
// denial stream with authz.denied rows would make a busy member look like an
// attacker.
//
// 0 (the zero value, and any negative) is UNLIMITED, matching the two booleans'
// rule: a profile that omits limits behaves exactly as one written before this
// field existed. Assigned subjects only — an unassigned member has no profile,
// so there is no cap to read and no global default one to fall back to (the
// `all` assignment IS the opt-in for a deployment-wide cap).
//
// ponytail: count-then-create, so two simultaneous creates can both see N-1 and
// both land. Accepted, exactly as the two 20-caps above accept it — the cap is a
// runaway-loop guard, not a licence meter, and a transaction around create just
// to make a soft cap exact is not worth the write path it would complicate.
func (s *Server) denyUserRunQuota(w http.ResponseWriter, r *http.Request, ceiling governanceCeiling) bool {
	return s.runQuotaRefusal(r, ceiling).write(s, w, r)
}

// validateImageBuildRequest enforces the image/devcontainer_repo XOR + the
// image-builder-wired requirement. Shared by decodeAndValidateCreateRun (the
// request's own --image) and seedRequestWorkspace's caller: a workspace's
// base_image can ALSO set req.Image, AFTER decodeAndValidateCreateRun has
// already run — so the identical check must run again once that seed lands,
// or a workspace's base_image would bypass a check an explicit --image must
// pass (e.g. silently pairing with a --devcontainer-repo the user set, or
// reaching FinalizeBase with no ImageBuilder wired).
// The two refusals are different causes (#656 M1) — the first is the
// caller's own request shape, the second a deployment capability the caller
// cannot fix by changing the request — so each gets its own reason.
func (s *Server) validateImageBuildRequest(req createRunRequest) (msg, reason string) {
	if req.Image == "" {
		return "", ""
	}
	if req.DevcontainerRepo != "" {
		return "image and devcontainer_repo are mutually exclusive", reasonImageDevcontainerExclusive
	}
	if s.cfg.ImageBuilder == nil {
		return "a custom sandbox image was requested but this control plane has no image builder wired " +
			"(start wardynd with -tags docker and set WARDYN_ENVBUILD_TOOLS_DIR / -envbuild)", reasonImageBuilderUnavailable
	}
	return "", ""
}
