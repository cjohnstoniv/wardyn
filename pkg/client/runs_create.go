// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CreateRunRequest is the body for POST /api/v1/runs.
type CreateRunRequest struct {
	Agent string `json:"agent"`
	Repo  string `json:"repo"`
	// Task is the agent's prompt for a non-interactive run. For an interactive
	// run it is instead the OPTIONAL boot seed: interpreted per
	// InteractiveStart (an initial prompt for "agent", a startup command for
	// "shell") and fired once, at sandbox boot, in the same persistent session
	// the human later attaches to — never re-run on attach. Empty is today's
	// pure-idle behavior, unchanged.
	Task string `json:"task,omitempty"`
	// Title is a short human NAME for the run. Runs that share a title are
	// grouped in the console's run list. OPTIONAL on the wire even though the
	// console requires it: the site-config probe, harness login and workspace
	// record/verify all create runs with no human to name them, and the console
	// falls back to Task for display. Trimmed and stored on the run row.
	Title string `json:"title,omitempty"`
	// Description is optional free-text context — why this run exists. Never
	// interpreted, only stored and displayed.
	Description string     `json:"description,omitempty"`
	PolicyID    *uuid.UUID `json:"policy_id,omitempty"`
	// ConfinementClass, when set, requests a specific confinement class
	// ("CC1"/"CC2"/"CC3"). Empty inherits the policy minimum; an unknown
	// non-empty value is rejected by the server with 400.
	ConfinementClass string `json:"confinement_class,omitempty"`
	// Interactive requests an interactive run: the sandbox comes up idle — or,
	// with a non-empty Task, runs that boot seed in a persistent session — so a
	// human can attach to it (wardyn run attach <id>) either way. Pair with a
	// never-reap policy (AutoStopAfterSec < 0) or the idle reaper will stop the
	// idle sandbox.
	Interactive bool `json:"interactive,omitempty"`
	// WorkspaceID, when set, launches the run against that ONBOARDED workspace:
	// the server prepends the workspace's stored source onto the resolved policy
	// (a repo as a workspace_repos entry, a local dir as a read-only-by-default
	// workspace_mounts entry) so the run inherits its approved egress, built image
	// and bound model/harness credentials. Composes with PolicyID / InlinePolicy /
	// the default policy — it seeds the source those cannot name without
	// hand-reproducing the workspace's exact path. Container-kind workspaces are
	// rejected: pass the image ref as Image instead.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
	// InlinePolicy, when set, supplies the run's full RunPolicySpec INLINE
	// instead of referencing a stored PolicyID. It is MUTUALLY EXCLUSIVE with
	// PolicyID (the server rejects both with 400); neither set falls back to the
	// configured default. The server validates it exactly like a stored policy
	// (mounts pass the same deny-list; api_key grants must reference an existing
	// secret) and attaches it with no stored policy id.
	InlinePolicy *RunPolicySpec `json:"inline_policy,omitempty"`
	// DevcontainerRepo, when set AND an image builder is wired (WARDYN_ENVBUILD),
	// triggers a devcontainer build of that git repo whose resulting image becomes
	// the sandbox image. Ignored (degrades to the convention image) when no builder
	// is wired. Mutually exclusive with Image.
	DevcontainerRepo string `json:"devcontainer_repo,omitempty"`
	// DevcontainerRef is the optional git ref (branch/tag/sha) to build for
	// DevcontainerRepo.
	DevcontainerRef string `json:"devcontainer_ref,omitempty"`
	// Image, when set, is a USER-supplied base image (Bring Your Own Image); the
	// server wraps it with the runner tools via a trusted finalize stage and
	// requires an image builder to be wired (WARDYN_ENVBUILD) — an explicit
	// Image with no builder wired is a hard 400 rather than a silent fallback.
	// Mutually exclusive with DevcontainerRepo.
	Image string `json:"image,omitempty"`
	// TaskMode selects how a non-interactive run executes Task: "" / "harness"
	// (default) runs the agent harness; "exec" runs Task as a plain shell
	// command in the same governed sandbox (no agent, no LLM credentials — the
	// BYOA/CI lane; see docs/CI.md). Ignored for an interactive run.
	TaskMode string `json:"task_mode,omitempty"`
	// InteractiveStart is TaskMode's interactive counterpart: what the attach
	// shell opens with. "" / "shell" drops the human into a shell in the
	// prepared workspace (the long-standing behavior); "agent" additionally
	// launches the image's agent CLI (claude / codex) there, once, on the first
	// attach. Request-scoped like TaskMode — never persisted on the run row,
	// carried to the sandbox as WARDYN_INTERACTIVE_START and consumed by the
	// image's attach ~/.bashrc. Ignored for a non-interactive run.
	InteractiveStart string `json:"interactive_start,omitempty"`
	// SeedAutoTools, when true, lets an interactive run's boot SEED (Task,
	// interpreted per InteractiveStart — see Task's own doc) use tools before a
	// human attaches, equivalent to --dangerously-skip-permissions for that
	// pre-attach span only. Default false: the seed is supervised, so an
	// unattended agent-started seed parks at its first tool-approval prompt
	// until someone joins. Meaningful only for an agent-started seed with
	// non-empty Task; request-scoped like InteractiveStart — never persisted on
	// the run row, carried to the sandbox as WARDYN_SEED_AUTO_TOOLS.
	SeedAutoTools bool `json:"seed_auto_tools,omitempty"`
	// ToolApprovals governs an AUTONOMOUS (non-interactive) Claude Code run's
	// own tool calls: "" / "auto" (default) is today's behavior — the sandbox
	// and egress policy are the only boundary, same as
	// --dangerously-skip-permissions; "hold" routes every tool call through
	// Wardyn's approval FSM instead, so an operator decides each one before it
	// runs. Rejected for codex-cli (no external tool-approval contract) and
	// ALSO rejected for an interactive run: that run's supervised-seed posture is
	// SeedAutoTools's job, so this field could never take effect there. It used
	// to be accepted and silently discarded, which is worse than refusing it —
	// the caller got a 201 and none of the supervision they asked for.
	// Request-scoped like TaskMode — never persisted on the run row, carried to
	// the sandbox as WARDYN_TOOL_APPROVALS.
	ToolApprovals string `json:"tool_approvals,omitempty"`
	// Workspaces carries PER-WORKSPACE options — which of a workspace's
	// OPTIONAL requirements (types.Workspace.Requirements, level="optional")
	// this run enables, and a read-only narrowing — for the workspaces this run
	// attaches. Additive to WorkspaceID, which stays a working SINGLE-selection
	// alias: a caller that sets only WorkspaceID (never touching Workspaces)
	// gets exactly today's behavior — no optional requirement enabled, no
	// narrowing. A REQUIRED requirement needs no entry here at all; it applies
	// automatically whenever its workspace is used. A selection naming a
	// workspace this run does not otherwise attach (via WorkspaceID or a
	// policy's workspace_mounts/workspace_repos) does nothing.
	Workspaces []WorkspaceSelection `json:"workspaces,omitempty"`
	// IntegrationID no longer chooses a model credential: a run naming one is
	// refused with a 422. Use ModelProvider.
	IntegrationID string `json:"integration_id,omitempty"`
	// ModelProvider chooses this run's model provider (GET /model-providers,
	// by id), ahead of a workspace's provider pin and the agent's default. It
	// must be one the caller is granted, that is on and that serves Agent —
	// otherwise the run is refused, never moved to another provider. Named on a
	// deployment with no model providers, or on a run that calls no model, it
	// is refused rather than ignored.
	ModelProvider string `json:"model_provider,omitempty"`
	// Drive opts this run into the member's USER DRIVE — the per-user storage
	// an admin allocated them. Nil (the default) mounts nothing, byte for byte
	// today. Nothing here names a path or a drive: the server resolves which
	// drive belongs to the authenticated caller and derives their home from
	// their own identity, so this flag can only ever ask for the storage the
	// caller was already granted.
	Drive *DriveSelection `json:"drive,omitempty"`
	// Components attaches custom components to this run: each entry names a
	// stored component by id or carries a run-only definition inline. Nil and
	// empty are the same and attach nothing, byte for byte today.
	Components []ComponentRef `json:"components,omitempty"`
	// Preset launches the named launch preset (see Preset): the server
	// expands it into the equivalent explicit request and runs the unchanged
	// create path under the caller's own ceiling. Alongside it only Title,
	// Task and PresetVersion may be set; any other field is refused.
	Preset string `json:"preset,omitempty"`
	// PresetVersion, with Preset, pins the version the caller expects: a
	// preset changed since is refused (409) rather than launched. 0 launches
	// the current version. The created run records the version it used.
	PresetVersion int `json:"preset_version,omitempty"`
}

// DriveSelection is the per-run user-drive option set. See
// CreateRunRequest.Drive.
type DriveSelection struct {
	// Enabled asks for the caller's drive to be mounted. False (and an absent
	// Drive) mounts nothing. A caller with no drive allocated, or one whose
	// governance profile carries DenyUserDrive, is refused rather than silently
	// given a run without it — asking for storage and not getting it is how
	// work is lost.
	Enabled bool `json:"enabled"`
	// ReadOnly, when set, NARROWS this run's mount: true forces read-only even
	// on a writable allocation. Nil leaves the resolved allocation's own posture
	// in effect.
	//
	// FALSE IS NOT A NO-OP EVERYWHERE, and the difference matters to a caller
	// scripting a run: against a READ-ONLY allocation it is REFUSED (422) and
	// never silently honoured — a run the caller believes is writable would only
	// reveal itself when the work failed to persist. On a WRITABLE allocation it
	// is a no-op, since that is the posture already in force.
	//
	// The same direction WorkspaceSelection.ReadOnly enforces, and for the same
	// reason: a run-level option may only ever narrow what an admin granted.
	ReadOnly *bool `json:"read_only,omitempty"`
}

// WorkspaceSelection is one per-run option set for an attached workspace. See
// CreateRunRequest.Workspaces.
type WorkspaceSelection struct {
	// WorkspaceID is the workspace this selection applies to (string form of
	// its uuid — matched against the run's referenced workspaces by id).
	WorkspaceID string `json:"workspace_id"`
	// EnabledOptional lists the workspace's OPTIONAL requirement KEYS (the
	// exact "<type>:<key>" form, e.g. "egress:api.stripe.com" or
	// "write:/home/user/repo") this run opts into. A key not listed here stays
	// at its safe default (no egress, no grant, read-only mount).
	EnabledOptional []string `json:"enabled_optional,omitempty"`
	// ReadOnly, when set, NARROWS this workspace's write:<path> requirements —
	// true forces every one read-only even if Required or enabled; false is a
	// no-op (a selection may only narrow what the contract already grants,
	// never widen it — see the fold in internal/api/runs_create.go). Nil
	// leaves the contract's own resolved default in effect.
	ReadOnly *bool `json:"read_only,omitempty"`
}

// CreateRunResult is the decoded POST /api/v1/runs 201 reply. AgentRun is
// EMBEDDED (the server puts the run's fields at the top level), so .ID/.State
// read straight off the result.
type CreateRunResult struct {
	types.AgentRun
	// Warnings are ADVISORY notices the server raised while resolving the run —
	// discouraged, never blocking: a member's inline policy narrowed to what
	// they are granted (an egress host, a secret-referencing grant, a workspace
	// repo), a workspace-directory collision with another active run, or an
	// ssh_key grant dropped because the agent has no SSH clone lane. Surface
	// them: the run is live either way, so a dropped warning is a silently
	// degraded run.
	Warnings []string `json:"warnings,omitempty"`
}

// CreateRun submits a new agent run and answers once the run row exists (state
// PENDING, never RUNNING) plus any advisory warnings; build and dispatch continue server-side.
// Status 201 on success; 400 on validation failure; 422 on policy/confinement
// mismatch; 503 when the runner is unavailable.
func (c *Client) CreateRun(ctx context.Context, req CreateRunRequest) (CreateRunResult, error) {
	var out CreateRunResult
	err := c.do(ctx, http.MethodPost, "/api/v1/runs", req, &out)
	return out, err
}

// PreflightItem is one row of the preflight setup checklist: something the run
// needs, and whether it is already satisfied. Only the fields callers render are
// modeled — the full server row additionally carries a structured UI "fix"
// action and a credential-residency label.
type PreflightItem struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Label      string `json:"label"`
	RequiredBy string `json:"required_by"`
	Status     string `json:"status"` // "satisfied" | "missing" | "unverified"
	Detail     string `json:"detail,omitempty"`
}

// PreflightResult is the decoded POST /api/v1/runs/preflight reply: the setup
// checklist plus the confinement class the run would ACTUALLY enforce after the
// policy floor and blast-radius raise.
type PreflightResult struct {
	SetupItems               []PreflightItem        `json:"setup_items"`
	EnforcedConfinementClass types.ConfinementClass `json:"enforced_confinement_class"`
	// Warnings carries resolveRunPolicy's clamp notes — a member's silently
	// narrowed inline policy, a filtered grant. The same list rides
	// CreateRunResult.Warnings at launch, so a preview and the real thing say
	// the same words about the same drop.
	Warnings []string `json:"warnings,omitempty"`
}

// Preflight DRY-RUNs a create-run request: the server resolves the policy
// through the same chokepoint launch uses (so an XOR violation, an unknown
// secret, or a non-onboarded workspace surface as the REAL launch error) and
// returns the setup checklist plus the enforced confinement class. It mints
// nothing, persists nothing, dispatches nothing. Pass the exact CreateRunRequest
// you would launch with. POST /api/v1/runs/preflight.
func (c *Client) Preflight(ctx context.Context, req CreateRunRequest) (PreflightResult, error) {
	var out PreflightResult
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/preflight", req, &out)
	return out, err
}
