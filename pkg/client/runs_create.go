// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
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
	// PolicyOverlay lays the policy keys it names onto the caller's default
	// policy (their governance ceiling, or the deployment default); a key it
	// leaves out keeps the default's value. A non-empty JSON object of
	// RunPolicySpec keys, never beside PolicyID or InlinePolicy (400
	// policy_overlay_conflict). The keys it names are bounded by the caller's
	// ceiling like an inline policy; the rest stay as the default has them. In a
	// run document it is written under intent.inline_policy.
	PolicyOverlay json.RawMessage `json:"policy_overlay,omitempty"`
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
	// Workspaces lists the workspaces this run attaches, in order, each with its
	// per-run options: which OPTIONAL requirements (types.Workspace.Requirements,
	// level="optional") it enables, a read-only narrowing, its in-sandbox
	// target and which of its repositories it clones. Every entry is attached;
	// Workspaces[0] is the PRIMARY. At most 32, each named once. WorkspaceID stays a working SINGLE-selection
	// alias: alone it attaches that one workspace with no option, and sent beside
	// Workspaces it must equal Workspaces[0] (400 workspace_selection_invalid
	// otherwise). A REQUIRED requirement needs no option; it applies whenever its
	// workspace is attached. An entry for a workspace the run's policy already
	// attaches (workspace_mounts/workspace_repos), other than the one WorkspaceID
	// names, adds its options only: its paths and repositories are the policy's,
	// so a target or repos there is 422.
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
	// AllowedImage chooses one of the images the organisation allows (the
	// preflight and preview `allowed_images`), by ref, instead of the resolved
	// default. Mutually exclusive with Image and DevcontainerRepo. Accepted and
	// validated; a server that cannot apply it yet refuses it with
	// request_field_unavailable, never ignores it.
	AllowedImage string `json:"allowed_image,omitempty"`
	// Placement is where the run's sandbox lives: "remote", the organisation's
	// own executor (the default), or "local", a runner the person registered.
	// Empty is "remote" until a single placement is eligible; "local" is
	// refused with placement_unavailable until the runner lane is enabled.
	Placement Placement `json:"placement,omitempty"`
	// RunnerID names the runner for Placement "local"; with more than one
	// runner online and none named the server answers 422 runner_ambiguous.
	RunnerID string `json:"runner_id,omitempty"`
	// RunnerPoolID names the pool the run starts on (RunnerPoolChoice.ID).
	// Choosing a pool grants no runner, credential or drive: it only narrows
	// where the run may go. Empty keeps today's placement; once the server
	// manages pools it applies the person's, then the organisation's, default.
	// Accepted and validated; a server that cannot honour pools yet refuses it
	// with request_field_unavailable, never ignores it.
	RunnerPoolID string `json:"runner_pool_id,omitempty"`
	// Resources is the CPU and memory this run asks for. A field set here beats
	// the source policy's own; a field left out keeps the policy's, else the
	// default. The server applies it within the strictest of the pool's, the
	// person's governance, the deployment's and (on their own runner) the
	// runner's limits: an over-ask is reduced to that cap, never refused.
	// CreateRunResult.ResourcesApplied and the dry runs say what was applied.
	Resources *RunResources `json:"resources,omitempty"`
	// Overrides are the person's per-component edits on the New Run cards
	// (OD-1), applied inside the resolve fold before the ceiling clamp. See
	// RunOverrides.
	Overrides *RunOverrides `json:"overrides,omitempty"`
	// MaxWaitSec is how long this run may wait for a slot when its runner pool is at its
	// concurrent-run limit, in seconds (1 to 604800; more is cut to 7 days with a warning). Absent
	// is 24 hours for a Background run and 1 hour for an Interactive one. At the end of the wait
	// the run is FAILED with the reason queue_timeout. Ignored by a pool with no limit. A pointer, so
	// a 0 is sent and refused rather than read as absent.
	MaxWaitSec *int `json:"max_wait_sec,omitempty"`
	// ModeContract marks a request as a 0.9 client's, whatever else it carries:
	// it is RunModeContractV09 and nothing else. The server then holds the
	// request to the run-mode contract, so an unset Experience is refused
	// (run_mode_required) and never read as an older client's. The console's New
	// Run wizard always sends it; an older client, and the CLI's older flags,
	// send none and keep the older mapping.
	ModeContract string `json:"mode_contract,omitempty"`
	// Experience is the canonical run mode, "background" or "interactive". A new
	// client sends it with the carriers below, which replace the legacy agent,
	// task, task_mode, interactive, interactive_start, seed_auto_tools,
	// tool_approvals and model_provider (sending both is run_mode_conflict). It
	// is required once any carrier is used; the server never infers it. It is
	// stored on the run, and every interactive door of a background run refuses.
	// A request with none of these fields is an older client's and is read as
	// before.
	Experience Experience `json:"experience,omitempty"`
	// Workload is what a background run executes: an agent task or a command.
	Workload *RunWorkload `json:"workload,omitempty"`
	// Tools are the included tool instances, each with its own provider or
	// configuration and its own tool rules. Distinct from Startup. A server that
	// cannot honour a shape yet (more than one tool, per-tool rules, a component
	// tool, an interactive run with none) refuses it with
	// request_field_unavailable, never ignores it.
	Tools []IncludedTool `json:"tools,omitempty"`
	// Startup is what an interactive run starts on its own: none, one included
	// harness or one command.
	Startup *RunStartup `json:"startup,omitempty"`
	// StartFolder is the run's one working directory. Accepted and validated; a
	// server that cannot apply an attachment folder yet refuses it with
	// request_field_unavailable.
	StartFolder *StartFolder `json:"start_folder,omitempty"`
	// NoRepositoriesOrDrives is the explicit choice to attach nothing. It cannot
	// be combined with any repository, workspace or drive. Nothing in the
	// sandbox is kept when the run ends unless it is pushed or copied out.
	NoRepositoriesOrDrives bool `json:"no_repositories_or_drives,omitempty"`
	// Shape is the New Run choice a run was set up from: agent_task,
	// environment or application. Optional; the server checks it against
	// experience, workload and tools and otherwise does not use it.
	Shape RunShape `json:"shape,omitempty"`
	// EndsAfterSec ends the run this many seconds after it is created, 1 to
	// 2147483647 (below 1 is a 400, run_end_after_too_small). An end past what the pool, the deployment and the person's
	// governance allow is cut to it, and the 201 says so; it is never refused. A
	// run with neither this nor NoEnd gets the effective default end. Setting both
	// is a 400 (run_end_conflicting_fields).
	EndsAfterSec *int `json:"ends_after_sec,omitempty"`
	// NoEnd asks for a run with no end. It is allowed only where the pool and the
	// deployment leave the lifetime unlimited and the person's governance permits
	// No end (allow_no_end with user_changes_limits); otherwise 403
	// run_end_no_end_not_allowed or run_limits_gate_denied.
	NoEnd bool `json:"no_end,omitempty"`
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
	// Target is where this workspace appears inside the sandbox. It must be an
	// absolute, cleaned path strictly under /home/agent with no ".." element,
	// and outside the drive roots. It names one place, so it applies to a
	// workspace that attaches exactly one repository or folder (see Repos);
	// otherwise it is 422. Empty keeps the workspace's own source targets.
	//
	// Every resolved target of the run is compared, not only the ones a request
	// names: equal or nested targets across different workspaces (a drive's
	// included), or two repositories cloning into one directory, are refused
	// with 422 workspace_target_overlap. Nesting inside one workspace's own
	// sources stays allowed.
	//
	// The PRIMARY workspace is Workspaces[0]: the run is listed under it. Every
	// attached workspace must agree on the model-provider pin, the base image
	// and the Azure DevOps organisation (422 workspace_pin_conflict,
	// image_conflict, workspace_ado_org_conflict).
	Target string `json:"target,omitempty"`
	// Repos picks which of this workspace's repository sources the run clones,
	// each at an optional ref. Nil: every repository source at its onboarded
	// ref. Present: exactly these, so an empty list clones none (a pointer, so
	// an explicit [] survives json.Marshal). A repo the workspace does not carry
	// is 422 workspace_repo_unknown. The workspace's other sources attach as
	// they would without it.
	Repos *[]RepoSelection `json:"repos,omitempty"`
}

// RepoSelection is one repository a WorkspaceSelection clones.
type RepoSelection struct {
	// Repo is the repository exactly as the workspace source stores it.
	Repo string `json:"repo"`
	// Ref is a branch, tag or full commit. Empty is the source's own ref, else
	// the remote's default branch. A ref that could read as a git option or a
	// revision expression (a leading "-", "..", "@{", control characters) is
	// 400 run_ref_invalid.
	Ref string `json:"ref,omitempty"`
}

// RunAttachment is one entry of the server's mount map for a run, as the
// preflight and the policy preview return it: what is mounted or cloned where.
type RunAttachment struct {
	// Kind is repository, folder (a host local_dir source, shown read-only in
	// Review) or drive.
	Kind        string `json:"kind"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Repo        string `json:"repo,omitempty"`
	// Ref is the ref the clone checks out; "" is the remote's default branch.
	Ref     string `json:"ref,omitempty"`
	DriveID string `json:"drive_id,omitempty"`
	// Name is the workspace or drive NAME, never a host path.
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	// StartsHere marks the attachment that holds the run's starting folder.
	StartsHere bool `json:"starts_here"`
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
	// ResourcesApplied is the CPU and memory the run asked for and got, and
	// which limit bound each.
	ResourcesApplied *AppliedResources `json:"resources_applied,omitempty"`
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
	// Components is what the run is given access to: its agent, the Git
	// providers its repositories live on and its attached components. Absent
	// for a run with none of them.
	Components []ComponentFact `json:"components,omitempty"`
	// Provenance explains which source contributed each resolved policy entry.
	Provenance []ProvenanceEntry `json:"provenance"`
	// Limits is what this run may get: the resolved pool's, the person's
	// governance and the deployment's limits for the run's type, folded.
	// ResourcesApplied is the CPU and memory it asked for and would get.
	Limits           *EffectiveRunLimits `json:"limits,omitempty"`
	ResourcesApplied *AppliedResources   `json:"resources_applied,omitempty"`
	// MainDrive is the main-drive size it asked for and would get, with its basis.
	MainDrive *MainDriveApplied `json:"main_drive,omitempty"`
	// LocalPlacement says per host, workspace source and component whether the
	// person's runner can honour it. Empty until the placement lane fills it.
	LocalPlacement []LocalPlacementFact `json:"local_placement"`
	// Image is the image the run would start from and where it comes from;
	// absent until the image lane resolves it. AllowedImages are the images the
	// organisation lets the person choose; empty until that lane fills it.
	Image         *ImageFact     `json:"image,omitempty"`
	AllowedImages []AllowedImage `json:"allowed_images"`
	// RunnerPool is the pool the request resolves to and how it was chosen;
	// absent until the pool lane resolves one. RunnerPools are the pools the
	// person may choose, each with whether it is available; empty until that
	// lane fills it. Neither is admission: a launch re-checks both.
	RunnerPool  *ResolvedRunnerPool `json:"runner_pool,omitempty"`
	RunnerPools []RunnerPoolChoice  `json:"runner_pools"`
	// BarrierOptions are the resolved pool's three barriers under this draft's
	// floors (policy_floor and access_floor appear only here); empty when unpooled.
	BarrierOptions []BarrierOption `json:"barrier_options"`
	// RunnerPoolSlots is the resolved pool's slot counts, with the run's max_wait_sec; absent when
	// unpooled. Advisory: the launch decides.
	RunnerPoolSlots *PoolSlots `json:"runner_pool_slots,omitempty"`
	// Access is what the run brings in: its rows, hosts, credentials, the line
	// for an unlisted host and the setup still needed. The policy preview
	// answers the same block for the same request.
	Access *AccessFacts `json:"access,omitempty"`
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
