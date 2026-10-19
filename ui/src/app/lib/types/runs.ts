/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Core run identity/state types (run-create shapes live in ./run-create).
// All wire fields are snake_case (see lib/types.ts's barrel comment for the
// one documented exception, in a different domain module).

import type { AutonomyLevel, RunLimits } from "../api/governance";
import type { PolicyRef } from "../api/health";
import type { PolicyContact } from "./site";
import type { Experience, RunOverrides } from "./new-run-contract";
import type { RunPolicySpec } from "./policy";
import type { RunnerPoolSelection } from "./runner-pools";

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

// The New Run choice a request was set up from (Go: client.RunShape, request field
// `shape`). The server checks it against experience, workload and tools
// (run_shape_conflict) and otherwise does not use it.
export type RunShape = "agent_task" | "environment" | "application";
export const RUN_SHAPES: readonly RunShape[] = ["agent_task", "environment", "application"];

// Where a run's sandbox lives (Go internal/types.Placement). "" is a run from
// before 0.9 and any run whose placement nothing recorded, and it is on the wire
// in both cases — so this is the full value set, not an optional field.
export type RunPlacement = "" | "local" | "remote";

// Who can attest a run's evidence (Go internal/types.RunEvidenceSource); "" is a
// legacy or unresolved record.
export type RunEvidenceSource = "" | "substrate" | "runner_asserted";

/** Go: types.RunNotRecorded. */
export type RunNotRecorded = "org_off" | "runner_declined";

export type RunState =
  // QUEUED: waiting for a slot in a capped runner pool; it holds no slot, identity or sandbox.
  | "QUEUED"
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

/** Go: types.RunQueue. `position` is how many runs wait ahead in the pool, never whose. */
export interface RunQueue {
  pool_id: string;
  queued_at: string;
  deadline_at: string;
  position: number;
  /** runner_pool_at_capacity, runner_pool_no_runner_free, runner_pool_runners_at_capacity or runner_pool_recording_store_off (RUN_WAIT_REASON). */
  reason: string;
}

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
  // Empty while the run is QUEUED; written when it starts.
  spiffe_id: string;
  runner_target: string;
  // Where the sandbox lives (0.9 client mode): "remote", a remote-hosted
  // runner, or "local", a runner the person registered. ALWAYS on the wire —
  // "" is a run from before 0.9 or one whose placement nothing recorded yet, so
  // this is a plain string union and not an optional field. Not
  // `confinement_source`, which says who CHOSE the class.
  placement: RunPlacement;
  // True when that placement was RESOLVED by the resolver rather than asked for
  // by name. Absent (false) for a legacy row and for a run whose placement was
  // the caller's own choice.
  placement_filled?: boolean;
  // The runner a local run landed on. Absent for a remote run and for a
  // pre-0.9 row; the Go side omits the key on NULL, never sends "".
  runner_id?: string;
  // Read-side facts about that runner (Go: AgentRun.RunnerName and the next two): its name, whether this
  // process has it connected, and when it last was. Never stored. The name is the one the run was created
  // under (a runner is never renamed), so a revoked runner's run keeps it; GET /runs/{id} also names a
  // remote-hosted runner, whose current name it reads. The other two are on a local run only. All are
  // absent when the runner could not be read, which is not the same as offline:
  // runner_online false is a fact, absent is "unknown".
  runner_name?: string;
  runner_online?: boolean;
  runner_last_seen_at?: string;
  // Who can attest this run's evidence: "substrate" (the organisation's own) or
  // "runner_asserted" (a runner said so, nobody can verify). Empty only for a
  // legacy or unresolved record, hence optional here.
  evidence_source?: RunEvidenceSource;
  // Why the run has no session recording (Go: AgentRun.NotRecorded), decided once at dispatch:
  // "org_off" (the organisation keeps none) or "runner_declined" (its runner does not record). Absent:
  // recorded, or a run from before 0.9.3.
  not_recorded?: RunNotRecorded;
  // The runner pool the run was admitted into, and how it was chosen (Go:
  // AgentRun.RunnerPool*), written once at create. A pool is a label, never
  // admission: the run goes to the exact runner in runner_id (self-hosted) or
  // remote_runner_id (remote-hosted). All absent for a run from before
  // pools and for a server-originated run that names none.
  runner_pool_id?: string;
  runner_pool_revision?: number;
  runner_pool_selection?: RunnerPoolSelection;
  remote_runner_id?: string;
  sandbox_ref?: string;
  // interactive runs come up idle (no agent task) so a human attaches and drives
  // them via the WS PTY; the runs board badges these as "awaiting attach".
  // Optional so an older backend payload (or a test fixture) without the field
  // still type-checks and degrades to autonomous.
  interactive?: boolean;
  // The canonical run mode chosen at launch (Go: AgentRun.Experience), the one
  // value every interactive door reads: a "background" run has no terminal, SSH,
  // interactive exec, web gateway or desktop. Absent for a run from an older
  // client or before 0.9, which keeps every door's behaviour. Not `interactive`,
  // which is the legacy execution flag.
  experience?: Experience;
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
  // store.go's SetRunDiskMiB (and migration 0096) for why this can't be
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
  // wait_budget_sec does not bound a run's start; see Start deadlines in
  // docs/OPERATIONS.md (#the-start-deadlines).
  ends_at?: string | null;
  // The latest end the run's pool and the deployment allow, measured from launch
  // (migration 0159, #1319). Absent when neither bounds the lifetime and for a
  // run from before it; an extension never goes past it.
  lifetime_cap_at?: string;
  // When the run left the pool queue (migration 0162); absent for a run that never waited.
  admitted_at?: string;
  // A waiting run's place in its pool's queue; present only while the run is QUEUED.
  queue?: RunQueue;
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
  // The run's end time, stamped by the three state writers that move a run to
  // a terminal state (migration 0092, #1197). Absent for a live run and for a
  // legacy row the backfill could not date exactly. A lease-ended run (
  // lost_reason "ended") stays RUNNING until the ended-run grace makes it
  // terminal, so ITS end time is lost_at, not this field — internal/types/
  // types.go's AgentRun.EndedAt doc. Nothing in the console reads this field
  // yet; it is a live wire field and the mirror rule forbids dropping one.
  ended_at?: string;
  // Set while a kept run's stop could not be confirmed (migration 0088,
  // #1060): the latest stop error and when containment first failed. The
  // lease sweep retries every pass and clears both once the stop lands.
  containment_error?: string;
  containment_error_at?: string;
  // When the re-clamp of a tightened profile last moved this run's end
  // (migration 0095, #573): the run page's "Your admin shortened the limit"
  // banner. Cleared when a person moves the end again.
  end_tightened_at?: string;
  // Set while the run's agent is frozen because nobody is there (migration
  // 0097, #572): "waiting" = parked on an open request, "idle" = unused past
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
  // AgentRun.Preset / PresetVersion (migration 0087, #1143): the launch
  // preset this run was expanded from, and which version. Absent for a run
  // sent as an explicit spec.
  preset?: string;
  preset_version?: number;
  // AgentRun.CreatedVia (migration 0094, #1142): the registered portal this
  // run was launched through on its owner's behalf. Absent for a run its
  // owner launched themselves.
  created_via?: string;
  // AgentRun.OperatorOwned (#1476): the run's owner is the operator itself
  // (admin token, local mode), not a person. A super admin may still open it
  // interactively; on a person's run only the owner may. Absent when false.
  operator_owned?: boolean;
  // internal/types/attention.go's RunAttention (#1197) — what this LIVE
  // run is waiting on and who (in the caller's own console view) can clear
  // it, projected only when the caller's GET /runs (or GET /me/attention)
  // request opted into the `view` contract. Absent on every other read, and
  // on a terminal or lease-ended run even under `view`. A SEPARATE named
  // interface (not inline), not just style: runs.wire.fields.test.ts's
  // tsInterfaceKeys scrapes an interface body LINE BY LINE with no brace
  // tracking, so an inline nested object's own keys (kind/by/pending) would
  // misread as top-level AgentRun keys and fail that parity test.
  attention?: RunAttention;
}

export interface RunAttention {
  kind: "approval" | "reauth" | "ado_consent" | "lost";
  by: "you" | "owner" | "admin";
  pending: number;
}

// PATCH /runs/{id}'s response (internal/api/run_end_wait.go's
// runEndWaitResponse, RL-4/RL-15): the run's end and wait AFTER the change,
// which may differ from what was asked — capped names the field(s) an
// over-ask was cut back to, and latest_end/max_wait_sec are the ceilings that
// decided it (absent when the caller's captured limits set no ceiling there).
export interface RunEndWaitResult {
  id: string;
  ends_at: string | null;
  wait_budget_sec: number;
  capped: string[];
  latest_end?: string;
  // Whose limit latest_end is, so the console can name it.
  latest_end_source?: "pool" | "governance" | "deployment";
  max_wait_sec?: number;
  // True only when the end was capped by the limit the run captured at launch
  // while the launch profile's own limit has since been loosened (#1322).
  // Display only; never sent as false.
  ends_cap_loosened?: boolean;
}

// POST /runs/{id}/revive's response (internal/api/run_revive.go's
// reviveResult, RL-10/RL-11): denied_added lists the host/egress denies the
// owner's CURRENT profile added over the run's frozen policy (empty when
// nothing new was blocked); agent_started is true only for a reboot/outage
// revive that also started the agent again (a proxy-only revive of a live
// run omits it).
export interface ReviveResult {
  run_id: string;
  denied_added: string[];
  proxy_release: string;
  agent_started?: boolean;
}

// GET /admin/runs/proxy-window's one row (internal/store's RunProxyRelease,
// F2/#580, PR #1317 review) — one live run whose proxy started on a release
// outside the supported N/N-1 window.
export interface RunProxyRelease {
  run_id: string;
  created_by: string;
  state: RunState;
  lost_reason?: "ended" | "reboot" | "outage";
  proxy_release: string;
}

// GET /admin/runs/proxy-window (internal/api/run_revive.go's
// handleAdminProxyWindow, RL-10, F2/#580): release is this daemon's own
// version; window names the supported minors, newest first; outside is every
// live run outside it — these are the runs "Restart with current limits"
// bulk-restarts.
export interface AdminProxyWindowResult {
  release: string;
  window: string[];
  outside: RunProxyRelease[];
}

// One run's outcome in POST /admin/runs/restart (internal/api/run_revive.go's
// adminRestartResult). lost_again is true when the restart itself left the
// run lost again (a 409 the person can act on — reviveEligible's "revive it
// from the run's page" arm for a rebooted run, since the bulk path never
// starts a stopped agent).
export interface AdminRestartResult {
  run_id: string;
  ok: boolean;
  error?: string;
  // The single-run revive's reason for the same refusal.
  reason?: string;
  lost_again?: boolean;
  denied_added?: string[];
}

export interface AdminRestartResponse {
  results: AdminRestartResult[];
}

// sync (GET /runs/{id} only) lists the wardyn-sync sessions open now on the
// replica that answers, from the gateway's live per-run sync budget; another
// replica's sessions are not listed. dir is the directory the client requested,
// not proof the sandbox started there. Ended sessions are the ssh.sync.transfer
// rows.
export interface RunSyncOpen {
  dir: string;
  opened_at: string;
}
export interface RunSync {
  open: RunSyncOpen[];
  // deny is the organisation's WARDYN_SYNC_DENY patterns, which `wardyn sync`
  // applies above every ignore file; the console does not show it yet.
  deny?: string[];
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
//
// user_type_name is the display name of AgentRun.user_type, resolved by the
// same handler (runUserTypeName) because GET /user-types is securityOps and a
// user-tier owner could not resolve the id themselves. Absent for a run with no
// type or a type since deleted — the Identity widget then shows no "Ran as".
//
// model_provider_name and model_provider_deleted (#996) do the same for
// AgentRun.model_provider_id: the provider's name (live, else the one run.create
// froze) and — only when true — that it has since been deleted. The server
// answers them for every viewer, so the page never infers "removed" from a
// setup status that lists just what the viewer's own agents use. Absent when
// the run names no provider or the server could not tell.
//
// created_via_name (#1234) is the display name of AgentRun.created_via, the
// portal that launched the run, resolved at read time (a revoked portal keeps
// its name) so a member who cannot read the portal registry still sees it.
// Absent for a self-launched run, or a portal the registry no longer holds.
//
// kept_until (#1320) is when an ended run's files are torn down, for the Ended
// banner. Present only while the run is RUNNING, ended by its own end and kept
// under a grace above 0; absent after a stop, a kill, a revive or a reboot.
export interface RunDetail extends AgentRun {
  ui_apps?: UIApp[];
  user_type_name?: string;
  model_provider_name?: string;
  model_provider_deleted?: boolean;
  created_via_name?: string;
  // The name of the pool the run was admitted into, as it reads now; a deleted pool is still named.
  runner_pool_name?: string;
  kept_until?: string;
  // The policy the run was launched under and how to ask for a change to it
  // (GET /runs/{id}, internal/policyref.Ref). Absent when none applies.
  policy?: PolicyRef;
  // Wardyn-sync sessions open now (GET /runs/{id} only); see RunSync.
  sync?: RunSync;
}

// GET /api/v1/runs/{id}/policy — the policy a run actually got (the run page's
// Policy tab). Mirrors the server's runPolicyResponse (internal/api/
// run_policy_view.go). `spec` strict-decodes as a RunPolicySpec, with values
// the reader may not see already blanked ("<redacted>") server-side.
export type RunPolicyState = "recorded" | "not_yet" | "never";
export type RunPolicySourceKind = "stored" | "inline" | "default" | "profile" | "unknown";

export interface RunPolicySource {
  kind: RunPolicySourceKind | (string & {});
  policy_id?: string;
  name?: string;
  deleted?: boolean;
  preset?: string;
  preset_version?: number;
  /** What the person changed on the New Run cards (the edits that stood, never a secret value); absent when nothing was edited. */
  overrides_diff?: RunOverridesDiff;
}

/** The recorded New Run edits; `clamped` lists, as "field value", the ones the ceiling then dropped. */
export type RunOverridesDiff = RunOverrides & { clamped?: string[] };

// The closed set of reasons launch changed a policy; an unknown one reads as "launch".
export type RunPolicyCause =
  | "workspace"
  | "source_control"
  | "component"
  | "mirror"
  | "model_access"
  | "git_broker"
  | "overlay"
  | "profile"
  | "org_disk"
  | "pool_disk"
  | "restart"
  | "limits"
  | "launch";

export interface RunPolicyChange {
  cause: RunPolicyCause | (string & {});
  // The RunPolicySpec json name the entries belong to.
  field: string;
  added?: string[];
  removed?: string[];
  // clamp_warnings lines, verbatim (cause "limits" only).
  detail?: string[];
  profile?: string;
  // Restart time (cause "restart" only).
  at?: string;
}

export interface StoredPolicyNow {
  state: "same" | "changed" | "updated" | "deleted";
  name?: string;
  updated_at?: string;
}

export interface RunPolicyView {
  run_id: string;
  state: RunPolicyState;
  recorded_at?: string;
  source: RunPolicySource;
  spec?: RunPolicySpec;
  // Some values are hidden from this reader.
  redacted: boolean;
  // Never null.
  changes: RunPolicyChange[];
  // False for a run that started before each change was recorded.
  complete: boolean;
  stored_policy_now?: StoredPolicyNow;
  // Which layer set each top-level policy value, keyed by its json name.
  provenance?: Record<string, PolicyProvenance>;
}

// internal/policyref.Provenance: the layer that set one value of a run's policy
// ("deployment", "profile" with the leaf's name, "member" or "operator"), the
// contact to ask, and locked once a layer forbids lower layers to change it
// (never set until walls or delegation can lock a knob).
export interface PolicyProvenance {
  layer: string;
  name?: string;
  contact?: PolicyContact;
  locked?: boolean;
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

// Who currently holds the run's shared tmux PTY (GET /runs/{id}/attach/holder).
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

// internal/store.RunCapacitySums — one set of configured-reservation sums
// (GET /admin/runs/capacity). `unknown` counts holding runs whose reservation
// was never recorded; they add to no sum.
export interface RunCapacitySums {
  holding: number;
  unknown: number;
  agent_cpu_request_millis: number;
  agent_cpu_limit_millis: number;
  agent_memory_request_mib: number;
  agent_memory_limit_mib: number;
  proxy_cpu_millis: number;
  proxy_memory_mib: number;
  proxy_cpu_uncapped: number;
  held_cpu_millis: number;
  held_memory_mib: number;
}

// internal/store.RunCapacityRunner: `requests` (Kubernetes) or `caps` (Docker).
export interface RunCapacityRunner extends RunCapacitySums {
  basis: "requests" | "caps";
}

export interface RunCapacityOwner {
  owner: string;
  holding: number;
  by_runner: Record<string, RunCapacitySums>;
}

export interface RunCapacityAge {
  bucket: string;
  count: number;
}

// internal/store.RunCapacityUnschedulable; null columns are unrecorded.
export interface RunCapacityUnschedulable {
  id: string;
  owner: string;
  waited_seconds: number;
  reason: string;
  runner_kind: string | null;
  agent_cpu_request_millis: number | null;
  agent_memory_request_mib: number | null;
  proxy_cpu_millis: number | null;
  proxy_memory_mib: number | null;
}

// internal/store.RunCapacity.
export interface RunCapacity {
  states: Record<string, number>;
  paused: number;
  kept: number;
  totals: RunCapacityRunner;
  age_buckets: RunCapacityAge[];
  by_runner: Record<string, RunCapacityRunner>;
  by_owner: RunCapacityOwner[];
  by_owner_truncated: boolean;
  unschedulable: RunCapacityUnschedulable[];
  unschedulable_total: number;
  /** Age in seconds since created_at of the oldest holding run (0 when none). */
  oldest_active_seconds: number;
}

// GET /api/v1/admin/runs/capacity: RunCapacity plus generated_at and basis
// (the handler envelope in internal/api/runs_capacity.go).
export interface RunCapacityResponse extends RunCapacity {
  generated_at: string;
  basis: "configured_reservations";
}
