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

export type WorkspaceKind = "local_dir" | "repo" | "ephemeral";
// The scan lifecycle. The import-pipeline stages a retired "Import v2" wave
// had widened this with (building/build_error/verifying/verify_failed/ready)
// are gone — the server collapsed every stored row back to this set — so a
// workspace is not-yet-scanned, mid-scan, scanned (ready to use), or errored.
// With attached sources the status is DERIVED: the worst of the attached
// sources' own statuses (error > scanning > pending_scan > scanned).
export type WorkspaceStatus = "pending_scan" | "scanning" | "scanned" | "error";

// One operator-approved (or scanner-detected) setup command run during the
// build/verify stage. `source` is provenance ("detected" | "operator" | …).
export interface SetupCommand {
  stage: string;
  command: string;
  source?: string;
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
  leak_findings?: { path: string; kind: string; line?: number; source?: string }[];
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

// The OPERATOR-owned model/harness credential BINDING on a workspace/
// container: a run that picks this workspace resolves its model access through
// the NAMED Integration (category ai_provider) — the generalized replacement
// for the retired inline {mode, api_key_secret, bedrock} shape (the server
// tolerates old stored rows by decoding them as "no binding"). Refs/names
// only, never secret values: the credential lives on the Integration, injected
// proxy-side at dispatch. Absent / "" => no binding; the run falls back to the
// global provider config. Set via createWorkspace's `llm_cred` (create) or
// api.setWorkspaceLLMCred (edit).
export interface WorkspaceLLMCred {
  integration_ref?: string;
}

// ---- The tier-1 source library + tier-2 image catalog (wire rows) ----
// Mirror internal/types/workspace_contract.go's Source / BaseImageEntry.

// One library source: a repo/dir configured ONCE — its own requirements
// contract, its own scan profile/status — attached to any number of
// workspaces. Identity (kind, locator, ref) is deduplicated server-side.
export interface Source {
  id: string;
  kind: "local_dir" | "repo";
  locator: string;
  ref?: string;
  name: string;
  requirements?: WorkspaceRequirementsMap;
  profile?: Record<string, unknown> | null;
  status: WorkspaceStatus;
  active_run_id?: string;
  created_at: string;
  updated_at: string;
}

// One catalog image: registry ref / custom recipe / BYO — shared across
// workspaces. "recommended" is never a catalog row (derived per workspace).
export interface BaseImageEntry {
  id: string;
  kind: "registry" | "custom" | "byo";
  name: string;
  image: string;
  steps?: string[];
  created_at: string;
  updated_at: string;
}

// ---- Composition + requirements-contract wire types ----
// Mirror internal/types/workspace.go + workspace_contract.go 1:1. The request
// shape doubles as the response shape (identical wire fields either way).
export type WorkspaceSourceKind = "local_dir" | "repo" | "ephemeral";

// One entry in a workspace's composition — a Workspace is one-or-more of
// these. Field relevance by kind: local_dir -> path/target/writable,
// repo -> source/ref/target, ephemeral -> target only.
export interface WorkspaceSourceInput {
  type: WorkspaceSourceKind;
  path?: string;
  source?: string;
  ref?: string;
  target?: string;
  writable?: boolean;
}

export type WorkspaceBaseImageKind = "recommended" | "registry" | "custom" | "byo";

export interface WorkspaceBaseImageInput {
  kind: WorkspaceBaseImageKind;
  // Required for every kind except "recommended".
  image?: string;
  // "custom" only — Dockerfile RUN/ENV/ARG lines layered on `image`.
  steps?: string[];
}

export type RequirementLevel = "required" | "optional";
export type RequirementProvenance = "scan_seeded" | "operator_set";

// One entry in a requirements contract, keyed "<secret|egress|write|integration>:<rest>"
// (split on the FIRST colon only — a write:<path> suffix may itself legally
// contain colons).
export interface WorkspaceRequirement {
  level: RequirementLevel;
  provenance: RequirementProvenance;
}
export type WorkspaceRequirementsMap = Record<string, WorkspaceRequirement>;

// One attachment in the three-tier model: this workspace mounts a shared
// library source (source_id) — or an inline ephemeral scratch row — at
// `target`, with per-ATTACHMENT writability.
// Ordering is load-bearing: attachments[0] is the primary.
export interface WorkspaceAttachment {
  source_id?: string;
  ephemeral?: boolean;
  target?: string;
  writable?: boolean;
}

// The contract a run against `ws` is actually held to: the server's fold of
// the attached sources' contracts under this workspace's own overlay
// (effective_requirements), with the overlay itself as the identity fallback
// for rows the hydrate pass hasn't materialized. Mirrors the server's own
// effectiveRequirements helper.
export function effectiveWorkspaceRequirements(ws: Workspace): WorkspaceRequirementsMap {
  return ws.effective_requirements ?? ws.requirements ?? {};
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
  // Opaque to the UI — internal/workspacescan owns the shape
  // (WorkspaceProfile: languages, package managers, egress domains, …).
  // Kept loosely typed; the needs panel does a typed cast-read (WorkspaceProfile).
  profile?: Record<string, unknown> | null;
  // Operator-owned egress allowlist for this workspace — unioned into a run's
  // allowlist at launch just like the profile's egress_domains, but these were
  // EXPLICITLY approved by the operator (from suggested_egress) rather than
  // auto-derived. Managed via api.setApprovedEgress (full-replacement PUT).
  approved_egress?: string[];
  image_ref?: string;
  // Operator-owned model/harness credential binding — see WorkspaceLLMCred.
  // Absent/mode="" => no binding; a picking run falls back to the global
  // provider config. Settable via createWorkspace (create) or
  // api.setWorkspaceLLMCred (standalone edit) — NOT via updateWorkspace.
  llm_cred?: WorkspaceLLMCred;
  status: WorkspaceStatus;
  // The record/verify run currently holding this workspace's slot, if any.
  active_run_id?: string;
  // Record step (optional, skippable): per-session recording outcomes, keyed by a
  // slug of the operator-chosen session name. Absent on a fresh workspace (no
  // sessions recorded yet) — the UI offers a "New session" affordance.
  record_results?: Record<string, RecordResult>;
  // ---- Composition (three-tier) ----
  // The derived sources view, in attachment order (sources[0] is primary).
  // Response-only; writes go through `sources` on create/update (upserted into
  // the shared library and attached) or the attachments themselves.
  sources?: WorkspaceSourceInput[];
  // The base-image choice (catalog row when bound; absent => the derived
  // recommended build).
  base_image?: WorkspaceBaseImageInput;
  base_image_id?: string;
  // This workspace's OWN requirement rows — the overlay. What a run is
  // actually held to is effective_requirements (the fold); read via
  // effectiveWorkspaceRequirements().
  requirements?: WorkspaceRequirementsMap;
  // The server-side fold of attached sources' contracts under the overlay —
  // read-only, recomputed at every read.
  effective_requirements?: WorkspaceRequirementsMap;
  // The three-tier attachment list (shared library sources + inline ephemeral
  // rows, with per-attachment writability).
  attachments?: WorkspaceAttachment[];
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
