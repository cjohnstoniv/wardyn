/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run preflight (POST /api/v1/runs/preflight): the setup readiness checklist,
// the dry-run result and the model-credential facts. Split from ./runs so new
// preflight twins have room; the shared run types stay there.

import type { AutonomyResolution } from "../api/governance";
import type { ComponentFact } from "./components";
import type { AccessFacts } from "./new-run-access";
import type { AllowedImage, AppliedResources, HarnessSetFact, ImageFact, LocalPlacementFact, MainDriveApplied, RunAttachment } from "./new-run-contract";
import type { ProvenanceEntry } from "./policy-preview";
import type { BarrierOption, EffectiveRunLimits, PoolSlots, ResolvedRunnerPool, RunnerPoolChoice } from "./runner-pools";
import type { ConfinementClass, RiskItem, RiskLevel } from "./runs";
import type { SCMAccess } from "./setup";

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
export type SetupFixAction = "add_secret" | "scan_workspace" | "sign_in" | "choose_model_provider" | "none" | (string & {});

// WHERE the credential this item concerns actually lives at run time, derived
// from the FINAL spec's own delivery mechanism (compose_setup.go's Residency
// doc comment): "proxy_injected" (an api_key grant — the value never leaves the
// wardyn-proxy sidecar), "resident_mount" (a host credential bind-mounted into
// the sandbox), "brokered_mint" (a github_token/git_pat grant minted/resolved
// at task time), "resident_env" (an env_secret grant: the value is in the
// sandbox environment for the whole run) or "resident_file" (a file_secret
// grant: the value is a file in the sandbox for the whole run). "none" is a
// workspace-integration row that delivers no credential on purpose (the system
// authenticates outside HTTP). Absent when not applicable (workspace/egress/backend
// rows carry no single credential).
export type SetupItemResidency =
  | "proxy_injected"
  | "resident_mount"
  | "brokered_mint"
  | "resident_env"
  | "resident_file"
  | "none"
  | (string & {});

export interface SetupFix {
  action: SetupFixAction;
  secret_name?: string;
  // A Workspace.id, NOT a source path — api.scanWorkspace takes an id.
  workspace_id?: string;
  // sign_in: what to sign in to (model_provider, azure_devops, azure_cli or aws_cli) and its id.
  target?: string;
  id?: string;
  // choose_model_provider: the included harness that has none.
  tool?: string;
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
// than crashing.
//
// Where these are read: the new-run rail's preflight block
// (new-run-rail.tsx's RunRail) renders overall_risk, enforced_confinement_class,
// warnings and the `missing`/`unverified` setup_items rows; a `missing` backend
// row, like a fresh 4xx or a `missing` llm_access row, holds Launch for that
// exact body for up to 60s (use-launch.ts preflightBlock); every other row is
// advisory. The field is a live server
// contract (compose_setup.go) read by the CLI and CI scripts too, and the
// mirror rule forbids dropping a wire field the daemon still sends.
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
  // modelCredentialFacts): the provider this body chose, its kind and its
  // residency. preflightIsCurrent compares the whole request body, so a verdict
  // for a different agent can never render.
  //
  // ABSENT for an exec / non-model run, for a run no provider serves, and on a
  // refusal (a refusal has no verdict to publish).
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
  // What the run is given access to (internal/api's componentFacts): the Git
  // providers its repositories live on, then its components in request order.
  // The preview's rows plus the credential verdicts only this door reads.
  // Absent for a run with no repository on a Git provider and no component.
  components?: ComponentFact[];
  // Why each entry of the resolved spec is there; the policy preview returns the same rows
  // for the same request. Absent on an older server.
  provenance?: ProvenanceEntry[];
  // What this run may get (the resolved pool's, the person's governance and the
  // deployment's limits for its type) and the CPU and memory it asked for and
  // gets. Absent on an older server.
  limits?: EffectiveRunLimits;
  resources_applied?: AppliedResources;
  // The main-drive size it asked for and gets; absent on an older server.
  main_drive?: MainDriveApplied;
  // Whether each host, workspace source and component can run on the person's
  // runner; empty until the placement lane fills it.
  local_placement?: LocalPlacementFact[];
  // The image the run would start from and its source (absent until the image
  // lane resolves it), and the images the organisation lets the person choose.
  image?: ImageFact;
  allowed_images?: AllowedImage[];
  // The run's included harnesses (absent for a run with none).
  harness_set?: HarnessSetFact;
  // The pool the request resolves to and how it was chosen (absent until the
  // pool lane resolves one), and the pools the person may choose, each with
  // whether it is available; empty until that lane fills it.
  runner_pool?: ResolvedRunnerPool;
  runner_pools?: RunnerPoolChoice[];
  // The resolved pool's three barriers under this draft's floors; empty when unpooled.
  barrier_options?: BarrierOption[];
  // The resolved pool's slot counts and this run's max_wait_sec; absent when unpooled. Advisory.
  runner_pool_slots?: PoolSlots;
  // What the run brings in (rows, hosts, credentials, the unlisted line, setup);
  // the policy preview answers the same block. Optional so an older server still types.
  access?: AccessFacts;
  // The run's mount map: each repository, folder and drive at the path the run
  // uses it at, the one holding the starting folder marked. The server always
  // sends the array; optional here so an older server still types.
  attachments?: RunAttachment[];
}

// Where a run's MODEL credential lands (internal/api.modelCredentialResidency).
// A vocabulary of PLACE, not mechanism: this rail's reader is deciding whether a
// credential may sit inside the sandbox they are about to grant.
export type ModelCredentialResidency = "proxy" | "sandbox";

// internal/api.modelCredentialFacts. Member-safe: the chosen provider's id and
// kind, and a place — no host, no secret name, no access-portal URL.
export interface ModelCredential {
  provider?: string;
  kind?: string;
  residency: ModelCredentialResidency;
}
