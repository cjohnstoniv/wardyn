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
func (s *Server) decodeAndValidateCreateRun(w http.ResponseWriter, r *http.Request) (createRunRequest, types.ConfinementClass, string, bool) {
	var req createRunRequest
	if !decodeStrict(w, r, &req) {
		return req, "", "", false
	}
	// Only agent is hard-required. Repo is OPTIONAL: an inline-policy run that
	// mounts a local host folder (WorkspaceMount target /work) has no git repo to
	// clone, so requiring a repo would block the local-folder wizard path. The
	// clone wiring in dispatch is already nil/empty-safe (it surfaces no repo env
	// when run.Repo is blank), so an empty repo simply runs in the mounted
	// workspace (or an empty one).
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return req, "", "", false
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
		return req, "", "", false
	}

	// Item 5 + HIGH-3 review fix, plus the 0.6 image/workspace capabilities:
	// the request-level fields a member does not get to choose freely (see
	// denyMemberRequest) — checked before the XOR/builder validation below so a
	// member's request is refused with 403, never a 400 that implies the shape
	// alone is the problem.
	if s.denyMemberRequest(w, r, req) {
		return req, "", "", false
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
		return req, "", "", false
	}

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return req, "", "", false
	}

	// task_mode is a tiny closed enum; reject anything else up front (fail
	// closed, same shape as confinement_class above).
	if req.TaskMode != "" && req.TaskMode != "harness" && req.TaskMode != "exec" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown task_mode %q (want harness or exec)", req.TaskMode))
		return req, "", "", false
	}

	// interactive_start is task_mode's interactive counterpart and gets the same
	// closed-enum treatment. NOTE there is deliberately no title requirement
	// here: the console requires one, but the site-config probe, harness login
	// and workspace record/verify all create runs with no human to name them,
	// so a 400 would break every one of them. Title is a display field; the
	// console is where it is required.
	if req.InteractiveStart != "" && req.InteractiveStart != "shell" && req.InteractiveStart != "agent" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown interactive_start %q (want shell or agent)", req.InteractiveStart))
		return req, "", "", false
	}

	// tool_approvals gates whether an AUTONOMOUS (non-interactive) Claude Code
	// run's own tool calls route to a Wardyn approval instead of running
	// unsupervised — same closed-enum treatment as task_mode/interactive_start
	// above.
	if req.ToolApprovals != "" && req.ToolApprovals != "auto" && req.ToolApprovals != "hold" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown tool_approvals %q (want auto or hold)", req.ToolApprovals))
		return req, "", "", false
	}
	// codex-cli has no external tool-approval contract (Part C's spike verified
	// that only as far as codex's own docs go) — refuse the request outright
	// rather than silently falling back to today's unsupervised skip-permissions,
	// which would contradict the "hold" the caller explicitly asked for.
	if req.ToolApprovals == "hold" && req.Agent == "codex-cli" {
		writeError(w, http.StatusBadRequest, "tool_approvals=hold is not supported for codex-cli (no external tool-approval contract)")
		return req, "", "", false
	}

	// Title/description are free text bound for a TEXT column and every run row
	// in the console — cap them at the door rather than discovering a 40KB
	// "title" in the list view. Generous enough that no real name is refused.
	// Runes, not bytes: the message says "chars", and a CJK/emoji title well
	// under the limit was refused with a byte count the operator couldn't
	// reconcile with what they typed.
	if utf8.RuneCountInString(req.Title) > maxRunTitleLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("title is too long (%d chars, max %d)", utf8.RuneCountInString(req.Title), maxRunTitleLen))
		return req, "", "", false
	}
	if utf8.RuneCountInString(req.Description) > maxRunDescriptionLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("description is too long (%d chars, max %d)", utf8.RuneCountInString(req.Description), maxRunDescriptionLen))
		return req, "", "", false
	}

	// A run-explicit integration_id must name a real, run-selectable
	// (AI-provider) integration — checked eagerly, before any run is created,
	// so a typo or a source-control / corporate-network id (operator-wide,
	// never run-selectable) fails loud here rather than silently resolving to
	// nothing at foldRunIntegration time (llmcred.go).
	if req.IntegrationID != "" {
		if in, ok := s.resolveIntegrationRef(r.Context(), req.IntegrationID); !ok || !types.AIProviderKind(in.Kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("integration_id %q does not name an AI provider integration", req.IntegrationID))
			return req, "", "", false
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
	return req, reqCC, warning, true
}

// denyMemberRequest is the REQUEST-LEVEL half of a member's launch gate: the
// fields the inline_policy clamp (resolveRunPolicy) never touches because they
// are not policy at all. Reports true — having written the 403 and an
// authz.denied audit event — when the run must not proceed; callers must return
// immediately. An operator is exempt in one line at the top, so nothing below
// ever costs them a store read.
//
// Three fields, three different answers:
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
//   - workspace NARROWS: launching against an onboarded workspace is something
//     every member could already do, so it stays allowed until an admin
//     enforces the kind. This gates req.WorkspaceID, the member's OWN choice —
//     never the workspace a stored policy or a scan linkage brings in, which is
//     admin-authored (the doctrine in OPERATIONS §Multi-user).
//
// A workspace's own base_image (seedRequestWorkspace, called AFTER this) is
// deliberately NOT gated here: it is operator-authored config (the workspace was
// onboarded through an operator-only route), never a member's own free-text
// choice, and seedRequestWorkspace only ever sets req.Image when the caller left
// it empty — so this check, run BEFORE that seeding, can never catch it.
// Workspaces have no equivalent devcontainer_repo seed at all.
func (s *Server) denyMemberRequest(w http.ResponseWriter, r *http.Request, req createRunRequest) bool {
	if s.isOperator(r.Context()) {
		return false
	}
	if req.DevcontainerRepo != "" {
		return s.denyMemberField(w, r, "runs.image", "byoi_member",
			"a custom devcontainer repo (devcontainer_repo) is operator-only; launch with the agent's convention image or an onboarded workspace's base image")
	}
	if req.Image != "" {
		granted, err := s.capGranted(r.Context(), capImage, req.Image)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "resolve capability: "+err.Error())
			return true
		}
		if !granted {
			return s.denyMemberField(w, r, "runs.image", "byoi_member",
				"image "+req.Image+" is not granted to you — ask an admin to grant the exact image ref, "+
					"or launch with the agent's convention image or an onboarded workspace's base image")
		}
	}
	if req.WorkspaceID != nil {
		allowed, err := s.capSeamAllowed(r.Context(), capWorkspace, req.WorkspaceID.String())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "resolve capability: "+err.Error())
			return true
		}
		if !allowed {
			return s.denyMemberField(w, r, "runs.workspace", "capability_"+capWorkspace,
				"you are not granted workspace "+req.WorkspaceID.String()+" — ask an admin for access, or launch without a workspace")
		}
	}
	return false
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
