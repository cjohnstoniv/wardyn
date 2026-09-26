/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Core run identity/state types + the run-create input/result shapes.
// All wire fields are snake_case (see lib/types.ts's barrel comment for the
// one documented exception, in a different domain module).

import type { AutonomyLevel, AutonomyResolution, RunLimits } from "../api/governance";
import type { SCMAccess } from "./setup";

// The backend emits dotted agent ids like "claude-code" / "codex-cli".
// Older mock data used "claude_code" / "codex". Keep the union open
// (string) so label mapping can tolerate both forms; the literals below
// are kept for editor hints / autocomplete only.
export type Agent =
  | "claude-code"
  | "codex-cli"
  | "claude_code"
  | "codex"
  | "cursor"
  | (string & {});

export type ConfinementClass = "CC1" | "CC2" | "CC3";

// One in-sandbox loopback HTTP app the UI gateway may relay (mirrors Go's
// internal/types/policy.go UIApp). Operator-authored via policy, never
// user-editable in the console. port is the in-sandbox port the app listens
// on; path is the landing path after the gateway's ticket handoff (empty
// means "/").
export interface UIApp {
  name: string;
  port: number;
  path?: string;
}

// Canonical weakest -> strongest ladder — the single place the Fence < Wall <
// Vault order is spelled out. It lives beside the type it enumerates because both
// layers need it: the barrier UI (cc-meta.ts's picker, matrix, and default-
// confinement's strongest-available scan) and the transport (api/core.ts's
// ccRank, which clamps a run's requested class up to its policy floor). lib/ may
// not import from components/, so a home under components/ forced a second copy.
export const CC_ORDER: ConfinementClass[] = ["CC1", "CC2", "CC3"];

export type RunState =
  | "PENDING"
  | "STARTING"
  | "RUNNING"
  | "WAITING_FOR_CONFIRMATION"
  | "COMPLETED"
  | "STOPPED"
  | "ARCHIVED"
  | "FAILED"
  | "KILLED"
  // Keep the union open so an unrecognized backend state degrades to a
  // neutral badge instead of crashing the console (see primitives.tsx).
  | (string & {});

export interface AgentRun {
  id: string;
  created_at: string;
  updated_at: string;
  created_by: string;
  agent: Agent;
  repo: string;
  task: string;
  // The run's human NAME. Runs sharing a title are grouped on the Runs board.
  // Optional so a legacy row, an older backend, or a system run (scan, harness
  // login, workspace record) still type-checks — see runHeadline, which is what
  // every display site must use rather than reading title or task directly.
  title?: string;
  // Optional free-text context: why this run exists. Shown on run detail.
  description?: string;
  policy_id?: string;
  confinement_class: ConfinementClass;
  state: RunState;
  spiffe_id: string;
  runner_target: string;
  sandbox_ref?: string;
  // interactive runs come up idle (no agent task) so a human attaches and drives
  // them via the WS PTY; the runs board badges these as "awaiting attach".
  // Optional so an older backend payload (or a test fixture) without the field
  // still type-checks and degrades to autonomous.
  interactive?: boolean;
  // The host working-directory the run's workspace is bind-mounted from. Surfaced
  // on the runs board so a workspace-directory collision is visible at a glance.
  workspace_path?: string;
  // The RESOLVED sandbox image the run dispatched with (convention image,
  // devcontainer build, workspace-built, or BYOI-wrapped) — provenance, shown
  // on run detail. Empty/absent for legacy rows.
  image?: string;
  // The scan/verify/record-only TRUSTED linkage (internal/types/types.go's
  // AgentRun.WorkspaceID) — set for a record/verify step run, nil for an
  // ordinary user run. Modeled here ONLY so runHasWorkspace (below) can read
  // it: a record/verify run's workspace_ids is always empty (handleCreateRun
  // is the only writer of that column; the four internal step-run call sites
  // leave it nil), so gating "Always" on workspace_ids alone wrongly disables
  // it for exactly the run kind whose server-side rule-5 tie-break accepts it.
  // Nothing else in the console reads this field — don't widen its use beyond
  // runHasWorkspace without re-reading that server-side rule.
  workspace_id?: string;
  // R4-F077: DERIVED, never stored server-side (internal/types.AgentRun's
  // matching comment) — has_recording/recording_bytes/recording_duration_sec
  // let the Recordings library build its list off GET /runs alone (opt-in via
  // ?include=recording_meta; runs.listRuns({includeRecordingMeta:true})),
  // without fetching every run's cast just to answer "does one exist, how
  // big, how long". has_recording is the ONLY "no recording" signal: a zero
  // (or absent) recording_duration_sec is a real, header-only cast that
  // captured no output, not "unknown" or "none" — never gate on it.
  has_recording?: boolean;
  recording_bytes?: number;
  recording_duration_sec?: number;
  // READ-ONLY denormalization of the onboarded workspaces this run resolved to
  // at create time (referencedWorkspaces over the widened spec) — mirrors
  // internal/types/types.go's AgentRun.WorkspaceIDs. Distinct from
  // workspace_id above. Empty/absent for a run that resolved to no workspace
  // (most demo/system runs) or a pre-existing row. Don't read this field
  // directly to gate "Always" — use runHasWorkspace(run), which also covers
  // workspace_id; see its doc for why.
  workspace_ids?: string[];
  // The TRUSTED run->library-source linkage for a per-source scan run (the
  // three-tier retarget) — internal/types/types.go's AgentRun.SourceID. The
  // scan-facts upload authorizes on it the way a workspace run authorizes on
  // workspace_id above. Nil for a user run; nothing in the console reads it
  // today, but it is a live wire field and the mirror rule forbids dropping one.
  source_id?: string;
  // The run's EFFECTIVE idle auto-stop cap, captured from the resolved
  // RunPolicySpec at creation (internal/types/types.go's
  // AgentRun.AutoStopAfterSec). 0/absent = never auto-stop; a negative value is
  // explicitly never (interactive). No console reader today — kept for mirror
  // parity, same reason as source_id above.
  auto_stop_after_sec?: number;
  // The run's EFFECTIVE ephemeral disk cap in MiB (internal/types/types.go's
  // AgentRun.DiskMiB, RL-13), written by a scoped update at dispatch — see
  // store.go's SetRunDiskMiB (and migration 0087) for why this can't be
  // captured at create like auto_stop_after_sec above it. 0/absent = no cap
  // resolved. No console reader today (the /runs/{id}/resources endpoint
  // computes the Sandbox widget's disk_cap_bytes from it server-side); kept
  // for mirror parity, same reason as source_id above.
  disk_mib?: number;
  // The docker exec id of the run's agent process (internal/types/types.go's
  // AgentRun.AgentExecID) — empty for exec-less substrates and before Exec
  // runs. Server/crash-recovery bookkeeping only; no console reader today, kept
  // for mirror parity, same reason as source_id above.
  agent_exec_id?: string;
  // The user type the run's creator resolved as at create time
  // (internal/types/types.go's AgentRun.UserType, migration 0080) — the chosen
  // type for a run launched in the user view, the stamped one otherwise. Empty
  // for a run with no human creator or created before the migration. No
  // console reader today, kept for mirror parity, same reason as source_id
  // above.
  user_type?: string;
  // Server-authored one-line reason for a pre-agent-start failure arm
  // (internal/types/types.go's AgentRun.FailureHint, migration 0044) — set
  // when the run never got as far as an exit code (e.g. workspace mount
  // unavailable). Absent/empty for a run that failed WITH an exit code, or
  // any non-FAILED run. Distinct from RecordRun.failure_hint in
  // ./workspaces.ts (a different failure arm on a different resource).
  failure_hint?: string;
  // What the SUBSTRATE says this run is waiting on while it is STARTING, in the
  // substrate's own "<component>: <Reason>[: <message>]" words
  // ("agent: ImagePullBackOff: …", "pod: Unschedulable: …",
  // "image: Pulling: <ref>") — internal/types/types.go's AgentRun.StatusDetail,
  // migration 0063. The server BLANKS it for any run that is not STARTING,
  // except a run that FAILED on a terminal reason (where the reason is the
  // failure), so the console may render it whenever it is present without
  // re-checking the state. Absent from a pre-0.7.6 daemon — every reader treats
  // absence as "no reason to show", never as an error.
  status_detail?: string;
  // The bare reason token out of status_detail ("ContainerCreating",
  // "ImagePullBackOff", "Unschedulable", "Pulling", "Pending"…), DERIVED at read
  // and never stored — internal/api/runs_status_detail.go's projectStatusDetail.
  // Read THIS to decide (is it terminal? which sentence?) and status_detail only
  // for the platform's own message; parseStatusDetail falls back to parsing the
  // string when a pre-0.7.6 daemon sends no token.
  status_reason?: string;
  // internal/types/types.go's AgentRun.AutonomyLevel (0.8, migration 0065) —
  // the level resolveRunAutonomy (#97) resolved this run to at create time.
  // Absent for a run under no profile, a profile with no rubric, or a run
  // created before this field existed. Nothing resolves or enforces it yet
  // (#99 is types/storage/mirrors only); this field exists so the console has
  // somewhere to read it the day #93 renders it.
  autonomy_level?: AutonomyLevel;
  // Run limits captured at create (migration 0072, #567): the lease end (null =
  // no end), the wait for a decision (absent = the deployment's approval
  // expiry), the owner's profile run limits and that profile's id (absent for
  // an unassigned or super-admin owner). Optional: a pre-0.8 daemon sends none.
  ends_at?: string | null;
  wait_budget_sec?: number;
  run_limits?: RunLimits;
  governance_profile_id?: string;
  // Set when the run lost its sandbox but is kept (migration 0073, #568):
  // "ended" = its end passed, so it is stopped with no network and its files
  // stay for the ended-run grace. "reboot" = its container exited under it
  // and is kept stopped; "outage" = its token lapsed, so its proxy was removed
  // and its agent left running with no network (#574). The run stays RUNNING
  // meanwhile.
  lost_at?: string;
  lost_reason?: "ended" | "reboot" | "outage";
  // When the re-clamp of a tightened profile last moved this run's end
  // (migration 0086, #573): the run page's "Your admin shortened the limit"
  // banner. Cleared when a person moves the end again.
  end_tightened_at?: string;
  // Set while the run's agent is frozen because nobody is there (migration
  // 0085, #572): "waiting" = parked on an open request, "idle" = unused past
  // its profile's pause_idle_after_sec. The run stays RUNNING; typing, an
  // exec, the request closing or POST /runs/{id}/resume thaws it. active_at is
  // the presence clock (absent = nothing stamped since create).
  paused_at?: string;
  paused_reason?: "waiting" | "idle";
  active_at?: string;
  // internal/types/types.go's AgentRun.ModelProviderID (migration 0076, #527) —
  // the id of the model provider chooseModelProvider (#526) resolved this run
  // to at create time. The KIND is not here (it can change later on the
  // provider row itself); it lives only on the run.create audit event's
  // model_provider snapshot. Absent for a run under no provider block, a
  // block serving no provider for the agent, or a run created before this
  // field existed.
  model_provider_id?: string;
}

// GET /runs/{id}'s response shape: AgentRun plus ui_apps, a field ONLY that
// endpoint sends (handleGetRun's anonymous wrapper struct, runs_policy.go) —
// the READ-ONLY denormalization of the run's EFFECTIVE policy ui_apps. The run
// row itself carries only policy_id, and an inline/default policy has no id to
// fetch, so the console reads this off the run payload rather than GET
// /policies/{id}. Absent (never present as []) when the run has no declared
// apps or the lookup failed server-side — both render the lane's "no apps"
// state. Split off AgentRun rather than left optional-on-everything: every
// LIST consumer (GET /runs, the board/table) is typed for a field it never
// receives, and a card built from list data must not silently type-check as
// having answered "no apps declared" for one it was never asked about.
export interface RunDetail extends AgentRun {
  ui_apps?: UIApp[];
}

// Live-run evidence reads (the run-detail cockpit's widgets). These mirror
// internal/api/run_files.go and internal/api/run_resources.go — hand-maintained,
// with no parity test between the two, so a field renamed on the Go side is a
// runtime TypeError the console only catches in e2e. Change both together.

// ONE changed path in a run's workspace (GET /runs/{id}/files).
export interface RunFileStat {
  path: string;
  // git's porcelain code, position padding trimmed: "M", "A", "D", "??", "MM"…
  // Absent when the file was in the diff but not in `git status`.
  status?: string;
  // Optional on purpose, and the widget must keep them optional: a count that
  // was never read — a binary file, an untracked file (numstat never lists
  // one) — is ABSENT, not 0. Rendering `+0 −0` for a binary asset the agent
  // just rewrote is a claim that nothing changed, which is false. Never
  // `?? 0` these at the render site.
  added?: number;
  deleted?: number;
  // git declined to count lines ("-" in numstat).
  binary?: boolean;
}

export interface RunFilesResult {
  // "git" when we read a work tree; "none" when the workspace genuinely isn't
  // one; "unknown" when git ran and failed for some OTHER reason (most often it
  // is missing from the sandbox image). The last two are deliberately distinct:
  // collapsing them made the console blame the workspace for an image problem.
  vcs: "git" | "none" | "unknown" | (string & {});
  files: RunFileStat[];
  /** The in-sandbox directory actually inspected. On vcs:"none" this is what
   *  separates "no repo here" from "we looked in the wrong place" — the mount
   *  target is configurable per workspace source. Show it. */
  path?: string;
  // We stopped counting. Always present, including when false.
  truncated: boolean;
}

// Sandbox resource usage (GET /runs/{id}/resources).
//
// EVERY metric is optional, and that is the entire point of this shape. gVisor
// (the Vault tier) presents a synthetic procfs/sysfs and may withhold the
// cgroup files, so a metric the sandbox did not report comes back ABSENT. The
// widget renders RUN_COCKPIT.metricUnavailable for it. A 0 here would tell an
// operator a governed workload is using no memory — do not default these.
export interface RunResources {
  /** 0–100, already divided by core count. */
  cpu_percent?: number;
  memory_used_bytes?: number;
  /** Only present when a real limit was readable — `memory.max: "max"` falls
   *  back to MemTotal, and stays absent if that was unreadable too. */
  memory_limit_bytes?: number;
  disk_written_bytes?: number;
  /** Space occupied now, not disk_written_bytes' running write total. Beside
   *  disk_cap_bytes it is the bytes that cap counts; without one, the
   *  sandbox's root filesystem, image included (run_resources.go diskReading). */
  disk_used_bytes?: number;
  /** The run's ephemeral disk cap, present ONLY when a driver enforces it AND
   *  disk_used_bytes was measured the way that enforcement counts — never a
   *  denominator for a number about other bytes. Absent means: render
   *  disk_used_bytes with no bar. */
  disk_cap_bytes?: number;
  process_count?: number;
}

// The ONE server->client control frame on the attach WebSocket, sent as a TEXT
// frame on connect (binary frames stay raw PTY bytes). The client must LEARN it
// is read-only from the server — asking a client to refrain from typing is a
// request, not a control. Sent on every connect, read_only:false included, so
// the terminal never has to infer its mode from silence.
export interface AttachModeMsg {
  type: "attach-mode";
  read_only: boolean;
  holder?: AttachHolder;
}

// Who currently holds the run's shared tmux PTY (GET /runs/{id}/attach-holder).
// Attach is a SHARED session: without this, opening the run page while a CLI
// holds it silently competes for the same PTY.
export interface AttachHolder {
  held: boolean;
  principal?: string;
  since?: string;
  cols?: number;
  rows?: number;
  source?: "web" | "ssh" | (string & {});
}

// Terminal run states: the run has finished and can no longer be killed.
// Mirrors the backend terminal guard in internal/api/runs.go. COMPLETED is the
// terminal-success state (agent exited 0) and MUST be included — omitting it
// left the Kill button enabled on finished runs (the backend then 409s).
export const TERMINAL_RUN_STATES: readonly RunState[] = [
  "COMPLETED",
  "STOPPED",
  "ARCHIVED",
  "FAILED",
  "KILLED",
];

export function isTerminalRunState(state: RunState): boolean {
  return (TERMINAL_RUN_STATES as readonly string[]).includes(state as string);
}

// The ONE display fallback for "what is this run called?", used everywhere a run
// is named (the board, the table, the run-detail h1, audit, recording).
//
// It has to be shared because NEITHER field is sufficient alone: an interactive
// run may carry no task at all (an idle run with no boot seed — see
// CreateRunInput.interactive_start), and every run created before titles
// existed — plus every system run — carries no title. A site that reads one
// field directly renders a bare "—" for half the runs on the board.
export function runHeadline(run: Pick<AgentRun, "title" | "task">): string {
  return (run.title ?? "").trim() || run.task || "—";
}

// Whether an `always` egress-approval decision has somewhere on THIS run to
// persist to — the ONE place that answers it, mirroring the server's own
// rule-5 tie-break (internal/api/approvals.go): workspace_ids first, falling
// back to workspace_id. NEITHER field is sufficient alone: an ordinary user
// run has workspace_ids but no workspace_id, while a record/verify step run
// is the reverse (its workspace_ids is always empty — see workspace_id's
// doc). Reading workspace_ids alone — as run-detail.tsx once did — shows
// Always disabled on a record/verify run with the false reason "this run
// isn't attached to one", when the server would accept it.
export function runHasWorkspace(run: Pick<AgentRun, "workspace_ids" | "workspace_id">): boolean {
  return (run.workspace_ids?.length ?? 0) > 0 || !!run.workspace_id;
}

// The fields the New Run wizard composes into a POST /api/v1/runs body. policy_id
// and inline_policy are MUTUALLY EXCLUSIVE (XOR); neither set => default policy.
export interface CreateRunInput {
  // Omitted for a governed command (task_mode=exec) whose target already
  // carries a real base image — an explicit `image`, or a selected workspace
  // with one: task_mode=exec runs no agent harness, so naming one there was a
  // formality (see agentRequirementError, server-side). Every other run still
  // requires it.
  agent?: Agent;
  repo: string;
  task: string;
  // The run's name (grouping key) and an optional note on why it exists.
  // Optional on the wire — the server accepts a titleless run so the CLI and
  // the server's own system runs keep working — but REQUIRED by the New Run
  // screen, which is where a human is present to name the thing.
  title?: string;
  description?: string;
  policy_id?: string;
  confinement_class?: ConfinementClass;
  interactive?: boolean;
  // Bring Your Own Image: a user-supplied base image the backend WRAPS with the
  // runner tools (FROM <image> + COPY tools + cleared ENTRYPOINT) before use.
  // Mutually exclusive with a devcontainer build. Omitted → the convention image.
  image?: string;
  // task_mode selects how a non-interactive run executes `task`: "" / "harness"
  // (default) runs the agent harness; "exec" runs `task` as a plain shell
  // command in the same governed sandbox — no agent, no LLM credentials.
  // Ignored for an interactive run. Omitted → "harness" (backward-compatible).
  task_mode?: "harness" | "exec";
  // task_mode's INTERACTIVE counterpart: what the attach shell opens with.
  // "agent" launches the image's agent CLI in the prepared workspace once, on
  // first attach; "shell" / omitted is a bare terminal there. Ignored for a
  // non-interactive run (the server drops it structurally).
  interactive_start?: "shell" | "agent";
  // Opt-in for an interactive run's agent-started boot seed (`task`,
  // interpreted per interactive_start above): true lets the seed use tools
  // before a human attaches, instead of parking at its first tool-approval
  // prompt until someone joins. Omitted/false = supervised (default).
  seed_auto_tools?: boolean;
  // Tool-approval posture for an AUTONOMOUS (non-interactive) Claude Code run:
  // "hold" routes every tool call through a Wardyn approval instead of running
  // unsupervised. Omitted (wire default "auto") is today's behavior. Rejected
  // by the server for codex-cli and structurally inert for an interactive run.
  tool_approvals?: "auto" | "hold";
}

// POST /api/v1/runs response: the created run's fields PLUS an optional advisory
// `warnings` list (e.g. a workspace-directory collision with another active run).
// The run still launched — warnings are surfaced (toast / inline notice) but they
// never block. Structurally assignable to AgentRun, so onCreated callbacks that
// expect an AgentRun keep working.
export type CreateRunResult = AgentRun & { warnings?: string[] };

// Deterministic risk grade + setup-readiness checklist — mirror
// internal/composer's RiskItem/SetupItem and internal/api's PreflightResult.
// Moved here from the (now-deleted) AI Run Composer's own types/compose.ts:
// the composer UI is gone (stage-1 refactor), but the grader/checklist these
// describe still runs server-side for the manual wizard's own preflight (POST
// /runs/preflight, wizard.tsx) and Record Mode's profile synthesis
// (types/profile.ts) — both of which survive it.

// The graded risk level for one config choice / the overall proposal.
export type RiskLevel = "low" | "medium" | "high";

// One DETERMINISTICALLY graded config choice. risk_level is Wardyn's grade —
// never the LLM's self-assessment.
export interface RiskItem {
  field: string;
  value: string;
  risk_level: RiskLevel;
  rationale: string;
  invariant_ref?: string;
}

// POST /api/v1/policies/grade response — composer.Grade of a bare spec with no
// run attached (the policy panel's live safety meter). Same wire fields as
// PreflightResult's risk pair, but NON-optional: the grade endpoint is new, so
// there is no older-server tolerance to preserve — it always returns both.
export interface PolicyGrade {
  risk_assessment: RiskItem[];
  overall_risk: RiskLevel;
}

// The proposed run scalars shared by the run-preview surfaces that echo them
// back for a human to review before anything launches: the (retired) AI Run
// Composer's proposal and Record Mode's profile synthesis (ProfileProposal,
// types/profile.ts). devcontainer_repo is composer-only, kept for that
// proposal shape's sake.
export interface ComposeRunProposal {
  agent: Agent;
  repo: string;
  task: string;
  confinement_class?: ConfinementClass;
  interactive?: boolean;
  devcontainer_repo?: string;
}

// Setup readiness checklist — mirrors internal/api/compose_setup.go's
// SetupItem/SetupFix EXACTLY (snake_case; FROZEN CONTRACT, same PR). Computed
// DETERMINISTICALLY from the FINAL post-clamp spec — never the model's
// self-assessment (same trust rule as risk_assessment above). v1 verification
// depth is declared-present only: "satisfied" means the referenced secret/
// workspace/grant IS THERE, not that Wardyn live-probed it actually works — so
// UI copy for it must say "configured", never "verified".
export type SetupItemKind =
  | "llm_access"
  | "secret"
  | "workspace"
  | "repo_credential"
  | "egress"
  // "backend": can THIS host enforce the proposal's confinement class right now
  // (setupBackendItem). "config_pair": a reconciled multi-field setting PAIR
  // (e.g. resolved subscription access <-> the credential-mount bless —
  // setupSubscriptionMountItem). Both are host/config state, not a credential
  // absence, so a "missing" row renders amber/neutral, never destructive
  // (unlike llm_access/secret).
  | "backend"
  | "config_pair"
  // A secret a mounted workspace's own files declare a need for. "missing" rows
  // carry a fix:{action:"add_secret", secret_name}. Deliberately NOT gated into
  // the destructive treatment the retired composer Review gave llm_access /
  // secret (step-review.tsx, deleted): the run still launches —
  // the workspace just may lack a credential it wants, an amber gap, not a red one.
  | "workspace_secret"
  // An integration a mounted workspace's requirements contract names as
  // Required. Config state like `backend`, not a credential absence, so a
  // "missing" row stays amber: the run still launches, it just cannot reach the
  // system it was promised. The runtime fold degrades silently by design (a
  // workspace may name an integration before it exists), which is exactly why
  // preflight has to say so.
  | "workspace_integration"
  | (string & {});
export type SetupItemStatus = "satisfied" | "missing" | "unverified" | (string & {});
export type SetupFixAction = "add_secret" | "scan_workspace" | "none" | (string & {});

// WHERE the credential this item concerns actually lives at run time, derived
// from the FINAL spec's own delivery mechanism (compose_setup.go's Residency
// doc comment): "proxy_injected" (an api_key grant — the value never leaves the
// wardyn-proxy sidecar), "resident_mount" (a host credential bind-mounted into
// the sandbox), or "brokered_mint" (a github_token/git_pat grant minted/resolved
// at task time). Absent when not applicable (workspace/egress/backend rows
// carry no single credential).
export type SetupItemResidency = "proxy_injected" | "resident_mount" | "brokered_mint" | (string & {});

export interface SetupFix {
  action: SetupFixAction;
  secret_name?: string;
  // A Workspace.id, NOT a source path — api.scanWorkspace takes an id.
  workspace_id?: string;
}

export interface SetupItem {
  // Stable "<kind>:<key>", e.g. "secret:anthropic-api-key".
  id: string;
  kind: SetupItemKind;
  label: string;
  required_by: string;
  status: SetupItemStatus;
  detail?: string;
  fix?: SetupFix;
  residency?: SetupItemResidency;
}

// POST /api/v1/runs/preflight response — a DRY-RUN of run-create's resolution +
// gating (mints/persists/dispatches nothing). setup_items is the SAME
// deterministic checklist derivation the (retired) composer Review used to
// show (deriveSetupItems); enforced_confinement_class is the class the run
// will ACTUALLY run at after the policy floor + blast-radius CC3 raise (may
// exceed the operator's pick when the run holds a write-capable / third-party
// production credential). risk_assessment/overall_risk are the SAME
// composer.Grade/OverallLevel output — optional for older-server tolerance:
// an absent value renders no risk panel and no acknowledgment gate rather
// than crashing. Advisory only, never a gate.
//
// Where these are read, honestly: the sole consumer is the new-run rail's
// preflight block (new-run-rail.tsx's RunRail), which renders overall_risk,
// enforced_confinement_class and warnings. `setup_items` has NO consumer — the
// five-step wizard's Review step (step-review.tsx) that used to render it was
// deleted with the wizard, and nothing replaced that surface. It is fetched on
// every Review and discarded; the field and its SetupItem subtree stay declared
// because they are a live server contract (compose_setup.go) and the mirror
// rule forbids dropping a wire field the daemon still sends. Whether to
// render it again is an open question, not decided here.
export interface PreflightResult {
  setup_items: SetupItem[];
  enforced_confinement_class: ConfinementClass;
  risk_assessment?: RiskItem[];
  overall_risk?: RiskLevel;
  // Clamp notices — non-empty only for a MEMBER whose inline_policy the server
  // bounded (composer.Clamp) or whose grant it dropped (filterUserGrants). The
  // same benign "Tightened by policy:" class the compose Review shows; here it
  // tells the member WHY the enforced policy differs from what they typed, since
  // launch itself stays silent. Absent on an older server that predates it.
  warnings?: string[];
  // WHERE this run's model credential will land (internal/api's
  // gradeModelCredential), graded from the lanes the create/Review mechanism gate
  // just resolved for THIS body. The rail prefers it over the /setup/status
  // harness row, which is the same grade taken against the deployment default
  // policy — and preflightIsCurrent compares the whole request body, so a verdict
  // for a different agent can never render.
  //
  // ABSENT for an exec / non-model run, for a caller whose roster read failed,
  // and on the 422 answered for a per_user member who has not signed in (a
  // refusal has no verdict to publish). The status row is the default path for
  // exactly that reason.
  model_credential?: ModelCredential;
  // Autonomy is what resolveRunAutonomy decided for THIS body (0.8 #97/#93) —
  // internal/api/preflight.go's preflightResponse.Autonomy. ABSENT (never a
  // zero value) when nothing bound the run: no assigned profile, no rubric on
  // it, or a rubric that leaves this posture's three fields unset — the same
  // condition under which the create audit row omits its own field.
  autonomy?: AutonomyResolution;
  // THIS caller's Azure DevOps access state (internal/api.SCMAccess, #386) —
  // deployment-wide, informational (the rail's "before you press Launch"
  // line), never the gate itself: a run that actually needs it and has none
  // 422s with reason "git_credential" instead. Absent when no Azure DevOps
  // row is configured at all.
  git_credential?: SCMAccess;
}

// Where a run's MODEL credential lands (internal/api.modelCredentialResidency).
// A vocabulary of PLACE, not mechanism: this rail's reader is deciding whether a
// credential may sit inside the sandbox they are about to grant.
export type ModelCredentialResidency = "proxy" | "sandbox" | "image" | "unknown";

// internal/api.modelCredentialFacts. Member-safe: a lane name, "shared" /
// "per_user", and a place — no host, no secret name, no access-portal URL.
export interface ModelCredential {
  mechanism?: string;
  residency: ModelCredentialResidency;
  credential_source?: string;
  staged_placeholder?: boolean;
}
