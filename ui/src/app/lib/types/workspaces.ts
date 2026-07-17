/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Onboarded workspaces — admin-gated GET/POST/PUT/DELETE /api/v1/workspaces +
// POST /api/v1/workspaces/{id}/scan. A workspace is a pre-registered, reviewed
// local directory or repo; run-creation pickers offer ONLY these (never a
// free-text host path) — see internal/api/workspace_refs.go's
// validateWorkspaceSources, the un-bypassable server-side gate this UI mirrors.
import type { ProfileObservations } from "./profile";

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
  // Operator opt-in: mount this workspace READ-WRITE in the import flow's
  // Record/Verify runs. Omitted/false => read-only (the safe default). When true,
  // a sandboxed agent's changes PERSIST to the host directory.
  writable?: boolean;
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

// A platform secret is referenced by NAME only — values are write-only and never
// returned by the API. listSecrets() yields these names.
export type SecretName = string;
