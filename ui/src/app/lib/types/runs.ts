/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Core run identity/state types + the run-create input/result shapes.
// All wire fields are snake_case (see lib/types.ts's barrel comment for the
// one documented exception, in a different domain module).

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
  // READ-ONLY denormalization of the onboarded workspaces this run resolved to
  // at create time (referencedWorkspaces over the widened spec) — mirrors
  // internal/types/types.go's AgentRun.WorkspaceIDs. Distinct from
  // workspace_id above. Empty/absent for a run that resolved to no workspace
  // (most demo/system runs) or a pre-existing row. Don't read this field
  // directly to gate "Always" — use runHasWorkspace(run), which also covers
  // workspace_id; see its doc for why.
  workspace_ids?: string[];
}

// ============================================================
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
  // OPTIONAL ON PURPOSE, and the widget must keep them optional: a count that
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
// run carries no task at all (the server ignores task for one, so the console
// stops sending it), and every run created before titles existed — plus every
// system run — carries no title. A site that reads one field directly renders a
// bare "—" for half the runs on the board.
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
  agent: Agent;
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
}

// POST /api/v1/runs response: the created run's fields PLUS an optional advisory
// `warnings` list (e.g. a workspace-directory collision with another active run).
// The run still launched — warnings are surfaced (toast / inline notice) but they
// never block. Structurally assignable to AgentRun, so onCreated callbacks that
// expect an AgentRun keep working.
export type CreateRunResult = AgentRun & { warnings?: string[] };

// ============================================================
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

// ============================================================
// Setup readiness checklist — mirrors internal/api/compose_setup.go's
// SetupItem/SetupFix EXACTLY (snake_case; FROZEN CONTRACT, same PR). Computed
// DETERMINISTICALLY from the FINAL post-clamp spec — never the model's
// self-assessment (same trust rule as risk_assessment above). v1 verification
// depth is declared-present only: "satisfied" means the referenced secret/
// workspace/grant IS THERE, not that Wardyn live-probed it actually works — so
// UI copy for it must say "configured", never "verified" (decision 3).
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
  // the destructive treatment (see step-review.tsx): the run still launches —
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
// than crashing. Advisory only — rendered on the wizard's Review step, never
// gating Review itself (only Launch, and only for a HIGH grade — see
// step-review.tsx's RiskPanel).
export interface PreflightResult {
  setup_items: SetupItem[];
  enforced_confinement_class: ConfinementClass;
  risk_assessment?: RiskItem[];
  overall_risk?: RiskLevel;
  // Clamp notices — non-empty only for a MEMBER whose inline_policy the server
  // bounded (composer.Clamp) or whose grant it dropped (filterMemberGrants). The
  // same benign "Tightened by policy:" class the compose Review shows; here it
  // tells the member WHY the enforced policy differs from what they typed, since
  // launch itself stays silent. Absent on an older server that predates it.
  warnings?: string[];
}
