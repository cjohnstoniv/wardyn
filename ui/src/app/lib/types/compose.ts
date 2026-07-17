/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer wire types — mirror internal/api/compose.go and
// internal/composer. The composer is ADVISORY: it returns a PROPOSED run setup
// for a human to review/approve through the unchanged createRun path. It never
// creates a run or mints a credential. Also carries the deterministic setup
// checklist (SetupItem) + preflight result the composer Review and wizard share.
import type { Agent, ConfinementClass } from "./runs";
import type { RunPolicySpec } from "./policy";
import type { Workspace, WorkspaceSelection } from "./workspaces";

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
  // DETERMINISTIC policy actions (what the engine did to the proposal). Rendered as
  // "Tightened by policy:".
  warnings?: string[];
  // The LLM's OWN advisory remarks — untrusted model prose, kept SEPARATE from
  // `warnings` so it is never shown as an enforced policy action (M7).
  model_notes?: string[];
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

// POST /api/v1/runs/preflight response — a DRY-RUN of run-create's resolution +
// gating (mints/persists/dispatches nothing). setup_items is the SAME
// deterministic checklist the composer Review shows (deriveSetupItems);
// enforced_confinement_class is the class the run will ACTUALLY run at after the
// policy floor + blast-radius CC3 raise (may exceed the operator's pick when the
// run holds write-capable credentials). Advisory only — rendered on the wizard's
// Review step, never gating.
export interface PreflightResult {
  setup_items: SetupItem[];
  enforced_confinement_class: ConfinementClass;
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
