/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ============================================================
// Wardyn API types — mirror the real REST API under /api/v1
// All wire fields are snake_case.
// ============================================================

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
  policy_id?: string;
  confinement_class: ConfinementClass;
  state: RunState;
  spiffe_id: string;
  runner_target: string;
  sandbox_ref?: string;
  // interactive runs come up idle (no agent task) so a human attaches and drives
  // them via the WS PTY; the fleet board badges these as "awaiting attach".
  // Optional so an older backend payload (or a test fixture) without the field
  // still type-checks and degrades to autonomous.
  interactive?: boolean;
  // The host working-directory the run's workspace is bind-mounted from. Surfaced
  // on the fleet board so a workspace-directory collision is visible at a glance.
  workspace_path?: string;
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

// --- Run policies (admin-gated config) ---
// git_pat = a stored Personal Access Token brokered to git for a non-GitHub host
// (Azure DevOps / GitLab / ...). Unlike api_key (proxy-injected, value never
// returned) the PAT value reaches git via the credential helper as a password.
export type GrantKind = "github_token" | "cloud_sts" | "api_key" | "git_pat";

export interface GrantSpec {
  kind: GrantKind | (string & {});
  // Kind-specific scope object; free-form JSON on the wire.
  scope?: Record<string, unknown>;
  ttl_seconds?: number;
  requires_approval: boolean;
}

// A single operator/policy-controlled host bind mount. Mirrors the wire shape
// types.WorkspaceMount exactly: source = host path, target = in-container path,
// read_only optional with the SAFE DEFAULT being read-only (omitted => RO).
export interface WorkspaceMount {
  source: string;
  target: string;
  read_only?: boolean;
}

// One onboarded-repo attachment on a run (mirrors types.WorkspaceRepo). Repos
// are re-cloned fresh per run — this is just "which onboarded repo, and where"
// (target omitted => the server's convention default, ~/work/<repo-name>).
export interface WorkspaceRepo {
  repo: string;
  target?: string;
}

// FirstUseMode controls how an unknown (unlisted) egress domain is handled:
//  - always_deny: hard-deny, never surfaced for approval
//  - deny_with_review: raise an approval + deny now; a retry passes once approved
//  - wait_for_review: raise an approval and HOLD the connection until decided
// The wire accepts the legacy boolean too (true=deny_with_review, false=always_deny);
// asFirstUseMode() normalizes either form.
// SUBSCRIPTION_OAUTH_SECRET is the sentinel secret name (mirrors
// types.SubscriptionOAuthSecret in Go) that marks subscription LLM auth on a
// recorded profile — it is NOT a real stored secret.
export const SUBSCRIPTION_OAUTH_SECRET = "anthropic-subscription-oauth";

export type FirstUseMode = "always_deny" | "deny_with_review" | "wait_for_review";

export function asFirstUseMode(v: unknown): FirstUseMode {
  if (v === true) return "deny_with_review";
  if (v === false || v == null || v === "") return "always_deny";
  if (v === "deny_with_review" || v === "wait_for_review" || v === "always_deny") return v;
  return "always_deny"; // unknown => fail closed
}

// firstUseRaisesApproval reports whether the mode escalates to a human (either review mode).
export function firstUseRaisesApproval(v: unknown): boolean {
  const m = asFirstUseMode(v);
  return m === "deny_with_review" || m === "wait_for_review";
}

// firstUseLabel is a short human label for review/summary surfaces.
export function firstUseLabel(v: unknown): string {
  switch (asFirstUseMode(v)) {
    case "wait_for_review":
      return "Ask & wait";
    case "deny_with_review":
      return "Ask";
    default:
      return "Off";
  }
}

export interface RunPolicySpec {
  allowed_domains: string[];
  denied_domains?: string[];
  first_use_approval: FirstUseMode;
  allowed_methods?: string[];
  min_confinement_class: ConfinementClass;
  eligible_grants?: GrantSpec[];
  auto_stop_after_sec?: number;
  // Operator/policy-controlled host bind mounts injected into the sandbox.
  workspace_mounts?: WorkspaceMount[];
  // Onboarded repos cloned fresh into the sandbox for this run. Parallel list to
  // workspace_mounts (local dirs stay mounts; repos get their own list — see
  // types.Workspace / internal/workspacescan).
  workspace_repos?: WorkspaceRepo[];
  // When true the proxy allows ANY non-denied public host: denied_domains still
  // wins, allowed_domains may be empty. The SSRF/private-IP guard and the
  // exact-host allowlist required for credential injection are UNCHANGED.
  allow_all_egress?: boolean;
}

// ============================================================
// Onboarded workspaces — admin-gated GET/POST/PUT/DELETE /api/v1/workspaces +
// POST /api/v1/workspaces/{id}/scan. A workspace is a pre-registered, reviewed
// local directory or repo; run-creation pickers offer ONLY these (never a
// free-text host path) — see internal/api/workspace_refs.go's
// validateWorkspaceSources, the un-bypassable server-side gate this UI mirrors.
// ============================================================
export type WorkspaceKind = "local_dir" | "repo";
// Widened for the guided Import flow (scan → build → verify → ready). The
// original three (pending_scan/ready/error) still mean what they did; the new
// transient/failure states drive the import panel's rail + polling.
export type WorkspaceStatus =
  | "pending_scan"
  | "scanning"
  | "scanned"
  | "building"
  | "build_error"
  | "verifying"
  | "verify_failed"
  | "ready"
  | "error";

// One operator-approved (or scanner-detected) setup command run during the
// build/verify stage. `source` is provenance ("detected" | "operator" | …).
export interface SetupCommand {
  stage: string;
  command: string;
  source?: string;
}

// One executed step of a verify run. exit_code 0 => that step passed. log_head /
// log_tail are already server-side MASKED excerpts (never a raw secret).
export interface VerifyStep {
  stage: string;
  command: string;
  // TRUE for the step currently executing on a streamed intermediate upload — it
  // has no meaningful exit_code yet.
  running?: boolean;
  exit_code: number;
  duration_ms?: number;
  timed_out?: boolean;
  log_head?: string;
  log_tail?: string;
}

// The outcome of a verify run. ran=false => it never executed (e.g. no runner);
// ok=true => every step exited 0. While a run is in flight the workspace stays
// `verifying` and this object accumulates: `done` is false/absent on intermediate
// progress uploads and true on the final one; `total` is how many commands will run.
export interface VerifyResult {
  ran: boolean;
  ok: boolean;
  done?: boolean;
  total?: number;
  steps: VerifyStep[];
  // Honest environmental-failure hint (server-classified from the first failing
  // step: e.g. exit 127 → toolchain missing, "Unknown host" → Maven proxy,
  // "permission denied" → GOTMPDIR/noexec-/tmp). Present only on a failed verify.
  failure_hint?: string;
}

// POST /api/v1/workspaces/{id}/verify/suggest-fix response (internal/api/verify_fix.go).
// The AGENTIC half of the verify-fix loop: a single free-text diagnosis (2-3
// sentences) a composer backend proposes for a FAILED verify. ADVISORY — the
// operator applies it via the existing approve-host / add-secret / edit-command
// affordances; it is never auto-applied.
export interface VerifyFixSuggestion {
  suggestion: string;
}

// One secret a workspace's own committed files declare a NEED for — NAMES ONLY,
// values are never read (part of WorkspaceProfile.required_secrets). `kind` is an
// advisory category ("postgres"|"stripe"|"aws"|…); `optional` marks a deploy-time
// / non-blocking secret. Untrusted content-derived, so the panel renders it with
// an explicit provenance caveat and no value affordances.
export interface SecretNeed {
  name: string;
  kind?: string;
  optional?: boolean;
}

// The deterministic scan profile a workspace's committed files yield
// (internal/workspacescan). ADVISORY + untrusted-content-derived: names and hosts
// only, never values. Read via a typed cast off the loosely-typed
// Workspace.profile below — the wire field stays Record<string,unknown> so a
// newer/older scanner shape never breaks the type; the panel does a typed cast-read.
export interface WorkspaceProfile {
  languages?: string[];
  package_managers?: string[];
  tools?: string[];
  // Auto-allowed at launch (unioned into the run's egress allowlist).
  egress_domains?: string[];
  git_remotes?: { github?: string[]; other_hosts?: string[] };
  has_devcontainer?: boolean;
  has_dockerfile?: boolean;
  confidence?: string; // "high" | "medium" | "low"
  needs_review?: boolean;
  source?: string;
  required_secrets?: SecretNeed[];
  services_needed?: string[];
  // Content-derived hosts — ADVISORY, NOT auto-allowed (the operator approves each
  // one, moving it into Workspace.approved_egress).
  suggested_egress?: string[];
  // Rel paths of real .env-style files — PRESENCE only, never their contents.
  secret_files_present?: string[];
  // Largest detected build heap in MiB — ADVISORY (e.g. 24576 for a 24GB build).
  // Surfaced as a "Build wants ~N GB memory" hint only when >= 4096.
  build_memory_mib?: number;
  // CONTENT-FREE suspected committed secrets: path/kind/line ONLY — there is NEVER
  // a value field. `kind` is a detector id ("aws-access-key" | "github-token" | …).
  leak_findings?: { path: string; kind: string; line?: number }[];
  // Scanner-DETECTED setup commands (build/install/test), proposed to the operator
  // in the import flow for approval/edit before they run in a verify.
  setup_commands?: SetupCommand[];
}

// One recordable task the import "Record" step offers — derived SERVER-SIDE at
// read time from the approved setup commands (workspacescan.DeriveRecordTasks),
// NEVER computed on the client. `nature` is the derived task flavour; `interactive`
// picks the exec mode (AUTO wardyn-verify vs an attached terminal); `commands` is
// the operator-approved command set an AUTO task runs (absent for custom/interactive).
// The outcome of one open recording SESSION (opaque JSONB, keyed by a slug of the
// operator-chosen session name on Workspace.record_results). Sessions are
// user-named and interactive (no derived build/test taxonomy). `label` is the
// original name; `observations` is EXACTLY the ProfileObservations shape
// profile-review renders. `secret_names_minted` are the secret names proven-used
// (render-derived from the run's minted grants). The honesty fields are
// load-bearing: `record_failed` + `failure_hint` on an empty capture (control-plane
// reachability, never "needs no egress"); `kernel_sensor_blind` flags a CC3 microVM
// where the syscall sensor can't see; `caveats` carries the undeclared-secret note.
export interface RecordResult {
  run_id: string;
  label?: string;
  mode: "auto" | "interactive";
  // A confined VERIFY session (default-deny egress, limited to approved) vs an
  // open learning session. Learning sessions list on Record; confined on Verify.
  confined?: boolean;
  // The auth the session ran with (operator's configured provider), saved so it's
  // visible and a verify replay reflects the same setup: subscription | api-key | none.
  llm_mode?: string;
  model?: string;
  status: "recording" | "recorded" | "record_failed";
  started_at?: string;
  finished_at?: string;
  steps?: VerifyStep[];
  observations?: ProfileObservations;
  secret_names_minted?: string[];
  egress_promoted?: boolean;
  kernel_sensor_blind?: boolean;
  failure_hint?: string;
  caveats?: string[];
}

export interface Workspace {
  id: string;
  name: string;
  kind: WorkspaceKind;
  // Host directory path (local_dir) or repo slug/clone URL (repo).
  source: string;
  // repo only: branch/tag/commit to clone. Optional.
  ref?: string;
  // Optional default in-container mount/clone target; a run selection may
  // override it. Omitted => the server's convention default.
  default_target?: string;
  // Opaque to the UI — core A (internal/workspacescan) owns the shape
  // (WorkspaceProfile: languages, package managers, egress domains, …).
  // Kept loosely typed; the needs panel does a typed cast-read (WorkspaceProfile).
  profile?: Record<string, unknown> | null;
  // Operator-owned egress allowlist for this workspace — unioned into a run's
  // allowlist at launch just like the profile's egress_domains, but these were
  // EXPLICITLY approved by the operator (from suggested_egress) rather than
  // auto-derived. Managed via api.setApprovedEgress (full-replacement PUT).
  approved_egress?: string[];
  image_ref?: string;
  status: WorkspaceStatus;
  // Operator-approved setup commands (from api.setSetupCommands) — the exact list a
  // verify run executes. Distinct from profile.setup_commands (the scanner's proposal).
  setup_commands?: SetupCommand[];
  // The last verify run's outcome (steps + logs). Present once a verify has run.
  verify_result?: VerifyResult;
  // The profile hash that was verified, and when — so a later config edit can be
  // detected as "needs re-verify".
  verified_profile_hash?: string;
  verified_at?: string;
  // The run currently building/verifying this workspace, if any.
  active_run_id?: string;
  // Record step (optional, skippable): per-session recording outcomes, keyed by a
  // slug of the operator-chosen session name. Absent on a fresh workspace (no
  // sessions recorded yet) — the UI offers a "New session" affordance.
  record_results?: Record<string, RecordResult>;
  created_at: string;
  updated_at: string;
}

// A run-creation-time selection of an onboarded workspace: WHICH workspace,
// plus an optional per-run target/read-only override. The wizard resolves
// workspaceId -> the onboarded Workspace's kind/source when composing the wire
// spec (see buildSpec in new-run/wizard-types.ts) — this type intentionally
// carries only the operator's per-attachment choices, not a duplicated copy of
// the workspace record.
export interface WorkspaceSelection {
  workspaceId: string;
  target?: string;
  readOnly?: boolean;
}

// The fields the New Run wizard composes into a POST /api/v1/runs body. policy_id
// and inline_policy are MUTUALLY EXCLUSIVE (XOR); neither set => default policy.
export interface CreateRunInput {
  agent: Agent;
  repo: string;
  task: string;
  policy_id?: string;
  confinement_class?: ConfinementClass;
  interactive?: boolean;
}

// POST /api/v1/runs response: the created run's fields PLUS an optional advisory
// `warnings` list (e.g. a workspace-directory collision with another active run).
// The run still launched — warnings are surfaced (toast / inline notice) but they
// never block. Structurally assignable to AgentRun, so onCreated callbacks that
// expect an AgentRun keep working.
export type CreateRunResult = AgentRun & { warnings?: string[] };

// A platform secret is referenced by NAME only — values are write-only and never
// returned by the API. listSecrets() yields these names.
export type SecretName = string;

// ============================================================
// AI Run Composer wire types — mirror internal/api/compose.go and
// internal/composer. The composer is ADVISORY: it returns a PROPOSED run setup
// for a human to review/approve through the unchanged createRun path. It never
// creates a run or mints a credential.
// ============================================================

// The graded risk level for one config choice / the overall proposal.
export type RiskLevel = "low" | "medium" | "high";

// One uploaded attachment — name + the file's TEXT content (read client-side).
// The control plane treats this as UNTRUSTED analyzer input; it is never fetched.
export interface ComposeAttachment {
  name: string;
  content: string;
}

// The run's working-directory source (composer.Workspace). REQUIRED on a compose
// request and OPERATOR-chosen — never picked by the LLM. "local" bind-mounts a
// host dir at the agent's working dir; "git" clones a repo; "ephemeral" runs in a
// scratch dir wiped on teardown.
export type ComposeWorkspaceKind = "local" | "git" | "ephemeral";
export interface ComposeWorkspace {
  kind: ComposeWorkspaceKind;
  path?: string; // local: host directory
  read_write?: boolean; // local: false => read-only (safe default)
  repo?: string; // git: repo slug or clone URL
}

// How the interactive clarify step behaves. "auto" (default): the model decides
// whether to ask questions first. "always": force at least one round. "skip":
// one-shot — straight to a proposal.
export type ComposeMode = "auto" | "always" | "skip";

// One answered clarifying question carried in the transcript between rounds (the
// UI accumulates and resends it — the server holds no compose session).
export interface ComposeQA {
  question: string;
  answer: string;
}

// POST /api/v1/runs/compose request body.
export interface ComposeRequest {
  prompt: string;
  // Legacy singular workspace scalar. Superseded by workspaceSelections (the
  // onboarded multi-select) — api.compose() sends this only as a fallback when
  // no selections were made (ephemeral default).
  workspace?: ComposeWorkspace;
  // Onboarded-workspace multi-select (same WorkspaceSelection the wizard's Basics
  // step uses). When present, api.compose() resolves each against the fetched
  // Workspace[] list and sends the WHOLE resolved array as `workspaces` on the
  // wire — its first entry is the primary that drives the analyzer (mirrors
  // internal/api/compose.go's ComposeRequest.Workspaces). An empty/absent list
  // falls back to the legacy singular `workspace` (ephemeral by default).
  workspaceSelections?: WorkspaceSelection[];
  attachments?: ComposeAttachment[];
  sources?: string[];
  backend?: string;
  mode?: ComposeMode;
  transcript?: ComposeQA[];
  round?: number;
  // Operator's upfront run-mode choice: true = interactive (sandbox comes up idle
  // for attach), false = background (agent runs the task). Authoritative — the
  // server enforces it on the proposal, overriding the model's guess.
  interactive?: boolean;
  // Explicit PER-RUN opt-in to Claude subscription mode: the server injects the
  // operator-staged read-only ~/.claude credential mount copies into the proposal
  // (Claude agents only, and only when the operator ceiling blesses them —
  // otherwise the proposal carries an honest warning naming the fix). Off = the
  // more governed api-key path (key never resident in the sandbox).
  useSubscription?: boolean;
  // Operator's Getting Started default tier, sent as a raise-only per-run MINIMUM
  // (the RAW persisted pick — no client clamp). The server raises the policy floor
  // to it, capped at what this host can enforce; weaker than the proposal is a
  // no-op. Absent = the policy minimum stands.
  confinementFloor?: ConfinementClass;
  // Client-owned stable id for this compose SESSION (one per describe-mode entry,
  // not per round) — the server holds no session state (decision 1: stateless
  // round-trip protocol), so the client mints it and resends it on every round.
  // Wire field is `session_id`; also sent as `compose_session_id` on the eventual
  // createRun so a launched run's audit row can be correlated back to the compose
  // conversation that produced it (see api.createRun).
  sessionId?: string;
}

// The proposed run scalars (composer.RunInput). Shaped so the UI can launch it
// via the unchanged createRun path. devcontainer_repo is composer-only.
export interface ComposeRunProposal {
  agent: Agent;
  repo: string;
  task: string;
  confinement_class?: ConfinementClass;
  interactive?: boolean;
  devcontainer_repo?: string;
}

// One DETERMINISTICALLY graded config choice (composer.RiskItem). risk_level is
// Wardyn's grade — never the LLM's self-assessment.
export interface RiskItem {
  field: string;
  value: string;
  risk_level: RiskLevel;
  rationale: string;
  invariant_ref?: string;
}

// One clarifying question the analyzer asks before proposing (composer.Question).
// options empty ⇒ a free-text answer; non-empty ⇒ choose from options (the UI
// ALSO always offers a free-text "Other"); multi ⇒ choose any vs choose one.
export interface ComposeQuestion {
  id: string;
  question: string;
  why: string;
  options: string[];
  multi: boolean;
  // OPTIONAL plain-language enrichment (novice↔expert), filled by the analyzer only
  // when confident. Inert display text (never graded); shown in an info popover so
  // most "what is this / is it safe?" questions need no follow-up.
  help?: string; // one-sentence plain definition
  risk?: string; // what the riskier answer costs
  examples?: string[]; // what each option concretely enables
  misconceptions?: string[]; // correct a likely wrong assumption
}

// The discriminated "the analyzer needs answers" response (kind:"questions").
export interface ComposeClarification {
  kind: "questions";
  questions: ComposeQuestion[];
  assumptions?: string[];
  notes?: string;
  round: number;
}

// POST /api/v1/runs/compose proposal response (kind:"proposal").
export interface ComposeResponse {
  kind: "proposal";
  proposed: {
    run: ComposeRunProposal;
    inline_policy: RunPolicySpec;
  };
  risk_assessment: RiskItem[];
  overall_risk: RiskLevel;
  summary: string;
  warnings?: string[];
  // Deterministic FINAL-state model-access verdict (compose.go reconcileLLMAccess).
  // provisioned=false ⇒ the run launches but its first model call 404s — the review
  // surfaces this as its OWN distinct destructive banner (non-blocking), separate from
  // the benign clamp notices in `warnings`. Absent for a non-LLM agent (nothing to verify).
  llm_access?: { provisioned: boolean; note: string };
  // Doctor-style setup checklist for THIS proposal (see SetupItem below). Absent
  // on an older server that predates this field — the review screen renders no
  // checklist section rather than crashing on it.
  setup_items?: SetupItem[];
}

// The compose endpoint returns EITHER clarifying questions OR a final proposal.
export type ComposeResult = ComposeClarification | ComposeResponse;

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
  // (e.g. use_subscription <-> the credential-mount bless — setupSubscriptionMountItem).
  // Both are host/config state, not a credential absence, so a "missing" row
  // renders amber/neutral, never destructive (unlike llm_access/secret).
  | "backend"
  | "config_pair"
  // A secret a mounted workspace's own files declare a need for. "missing" rows
  // carry a fix:{action:"add_secret", secret_name}. Deliberately NOT gated into
  // the destructive treatment (see compose-review.tsx): the run still launches —
  // the workspace just may lack a credential it wants, an amber gap, not a red one.
  | "workspace_secret"
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

// POST /api/v1/runs/compose/assist — the ESCALATION-only help agent. ADVISORY:
// it answers the operator's free-text question with the current step's structured
// context in view (never pixels). Reuses the composer backend + hardened client;
// the answer is inert display text — never re-graded, never widens policy. Fires
// ONLY when the operator clicks "Ask something else", not on the default path.
export interface ComposeAssistContext {
  step: "describe" | "clarify" | "review";
  // All context is OPTIONAL so each surface passes only what it has (the describe
  // panel has prompt+workspace; the review screen has proposalSummary; etc.). The
  // server treats every field as advisory and answers from whatever is present.
  prompt?: string;
  workspace?: ComposeWorkspace;
  backend?: string;
  transcript?: ComposeQA[];
  round?: number;
  currentQuestion?: string; // clarify: the question the operator is viewing
  notes?: string; // clarify: the analyzer's notes for this round
  proposalSummary?: string; // review: the proposal summary
}
export interface ComposeAssistRequest extends ComposeAssistContext {
  question: string; // the operator's free-text question
}
export interface ComposeAssistResponse {
  answer: string;
}

// One configured composer backend (composer.BackendInfo). The default is
// preselected in the Describe-mode provider dropdown.
export interface ComposerBackend {
  name: string;
  provider: string;
  model: string;
  is_default: boolean;
}

// ============================================================
// Recording-Mode profile synthesis — POST /api/v1/runs/{id}/profile.
// ADVISORY + read-only: Wardyn replays a recording run's observed behaviour
// (egress, exec, file writes, connects) into a PROPOSED least-privilege run +
// inline_policy for a human to review and optionally save as a stored policy. It
// never creates a run or mints a credential.
// ============================================================

// One observed egress host: the HTTP methods seen and the allow/deny/pending
// decision tallies the proxy recorded for it during the recording run.
export interface ProfileDomainObservation {
  host: string;
  methods?: string[];
  allow_count: number;
  deny_count: number;
  pending_count: number;
}

// The raw, deterministic observations the synthesis is derived from. anomalies is
// the highlighted "something unexpected happened" channel (e.g. a denied host the
// agent kept retrying, an exec the profile can't explain).
export interface ProfileObservations {
  domains: ProfileDomainObservation[];
  minted_grant_ids: string[];
  exec_argv0s: string[];
  file_writes: string[];
  connects: string[];
  anomalies: string[];
}

// POST /api/v1/runs/{id}/profile response (kind:"profile_proposal"). proposed.run
// reuses the composer RunInput shape; inline_policy is the same RunPolicySpec the
// compose-review screen already renders.
export interface ProfileProposal {
  kind: "profile_proposal";
  proposed: {
    run: ComposeRunProposal;
    inline_policy: RunPolicySpec;
  };
  risk_assessment: RiskItem[];
  overall_risk: RiskLevel;
  observations: ProfileObservations;
  warnings?: string[];
}

export interface RunPolicy {
  id: string;
  name: string;
  created_at: string;
  updated_at: string;
  spec: RunPolicySpec;
}

// ============================================================
// First-run setup — GET /api/v1/setup/status (mirrors internal/api/setup.go
// SetupStatus). FROZEN CONTRACT — keep in exact sync with the Go struct.
// The wizard derives its per-step "done" state from these fields.
// ============================================================
export type SetupCheckStatus = "ok" | "warn" | "fail" | "info";
export type SetupCheckPlatform = "linux" | "darwin" | "windows" | "wsl" | "any";

// One environment/readiness row. status "info" is a permanent, non-fixable
// condition (e.g. no /dev/kvm on macOS) — render it informationally, never as a
// clearable warning.
export interface SetupCheck {
  id: string;
  label: string;
  status: SetupCheckStatus;
  platform?: SetupCheckPlatform;
  detail?: string;
  fix?: string;
}

// Boot-snapshot readiness of one configured composer backend. key_secret is a
// secret NAME (never a value); key_resolved is whether it was present at boot.
export interface ComposerBackendReadiness {
  name: string;
  provider: string;
  model: string;
  wire: string;
  // Normalized transport: HTTP wires => "api"; otherwise the cli tool / fake
  // variant. Absent when the backend didn't report one.
  transport?: string;
  // openai/azure backends only: "apikey" | "entra". "" for other providers.
  auth?: string;
  enabled: boolean;
  needs_key: boolean;
  key_secret?: string;
  key_resolved: boolean;
}

// A resident coding-agent CLI detected on the wardynd host PATH. logged_in is
// ADVISORY (a home-dir credential-file heuristic).
export interface SetupProvider {
  tool: "claude" | "codex" | (string & {});
  installed: boolean;
  logged_in: boolean;
  login_detected_via?: string;
  // How the CLI authenticates, when detectable: "subscription" (a resident Claude
  // OAuth token is present — fresh OR expired; freshness lives in the llm_provider
  // check detail, not here). "api_key" is reserved in the contract but never
  // inferred for a CLI; codex stays "" (no auth-file parse). Absent => unknown.
  auth_mode?: "subscription" | "api_key" | (string & {});
}

// Amazon Bedrock Anthropic-transport readiness (an enterprise "Connect a
// model" path — no direct Anthropic egress, billed via AWS). region/model are
// non-secret boot-time operator config, safe to echo; creds_present is a bool
// derived from secret-name presence (the AWS credential VALUES are never
// echoed, same as every other secret in this contract).
export interface SetupBedrock {
  region?: string;
  model?: string;
  creds_present: boolean;
}

export interface SetupStatus {
  ready: boolean;
  checks: SetupCheck[];
  auth: { mode: "local" | "sso" | "token" | "disabled"; local_loopback: boolean };
  runner: {
    driver: "docker" | "none" | (string & {});
    confinement_classes: ConfinementClass[];
    confinement_substrates?: Record<string, string>;
  };
  composer: { enabled: boolean; default?: string; backends: ComposerBackendReadiness[] };
  providers: SetupProvider[];
  secrets: { present: string[]; github_app: boolean };
  age_key: { durable: boolean };
  restart_required: boolean;
  restart_reason?: string;
  has_runs: boolean;
  platform: { os: string; wsl: boolean; kvm?: boolean };
  // Optional: absent on an older/fallback status (e.g. READY_FALLBACK, or a
  // daemon build that predates this field) rather than a required breaking
  // change to every existing SetupStatus fixture.
  bedrock?: SetupBedrock;
  // Whether wardynd itself sees a resident Claude login (host mode) vs is blind to
  // it (compose/container). host_like === false is why the LLM-access check reads
  // "no login" in compose even when the operator IS logged in on the host. Optional
  // for the same fixture-compat reason as `bedrock`.
  deployment?: { host_like: boolean };
}

// ============================================================
// Site config — the operator-wide baseline every run inherits (mirrors
// internal/types.go's SiteConfig EXACTLY, incl. json tags). GET/PUT
// /api/v1/site-config (admin-gated write). Secret VALUES are never
// included on the wire — only the ref NAMES the broker/proxy resolve at
// dispatch/injection time.
// ============================================================
export interface ArtifactOverride {
  base_url: string;
  token_secret_ref?: string;
}

export interface SiteConfig {
  upstream_proxy_secret_ref?: string;
  artifact_overrides?: Record<string, ArtifactOverride>;
  scm_hosts?: string[];
}

export type ApprovalKind = "credential" | "egress_domain" | "tool_call";

export type ApprovalState = "PENDING" | "APPROVED" | "DENIED" | "EXPIRED";

export interface ApprovalRequest {
  id: string;
  run_id: string;
  grant_id?: string;
  kind: ApprovalKind;
  // Real wire field is free-form JSON; keep `unknown` and let the screens
  // narrow it. Index signature lets UI read arbitrary keys safely.
  requested_scope: Record<string, unknown>;
  state: ApprovalState;
  requested_at: string;
  decided_at?: string;
  decided_by?: string;
  minted_jti?: string;
  reason?: string;
}

export type ActorType = "human" | "agent" | "system";

export type Outcome = "success" | "failure" | "denied";

export interface AuditEvent {
  id: string;
  time: string;
  run_id?: string;
  actor_type: ActorType;
  actor: string;
  action: string;
  target?: string;
  outcome: Outcome;
  source_ip?: string;
  data?: Record<string, unknown>;
}

// --- Run detail supporting shapes (UI-side, projected from audit events) ---
export interface CredentialGrant {
  id: string;
  scope: string;
  audience: string;
  state: "active" | "expired" | "revoked";
  minted_at?: string;
  expires_at?: string;
  jti?: string;
}

export interface EgressDecision {
  id: string;
  time: string;
  domain: string;
  decision: "allow" | "deny" | "pending";
  bytes?: number;
}

export interface AsciicastHeader {
  version: number;
  width: number;
  height: number;
  title?: string;
}
export type AsciicastEvent = [number, "o", string];
export interface Recording {
  run_id: string;
  header: AsciicastHeader;
  events: AsciicastEvent[];
  /** Raw asciicast v2 text, fed verbatim to asciinema-player for true terminal
   *  emulation (escape sequences interpreted, not printed). */
  cast: string;
}
