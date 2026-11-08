/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ComponentFact } from "./components";
import type { AccessFacts } from "./new-run-access";
import type { AllowedImage, AppliedResources, HarnessSetFact, ImageAdmissionFact, ImageFact, LocalPlacementFact, MainDriveApplied, RunAttachment } from "./new-run-contract";
import type { RunPolicySpec } from "./policy";
import type { EffectiveRunLimits, PoolSlots, ResolvedRunnerPool, RunnerPoolChoice } from "./runner-pools";

export type PolicyPreviewPending =
  | "task"
  | "model_provider_selection"
  | "credential_liveness"
  | "autonomy"
  | "tool_approvals"
  | "runner_confinement"
  | "drive_readiness"
  | "dispatch_egress";

export interface PolicyPreviewSource {
  kind: "default" | "unit" | "stored" | "inline";
  name?: string;
  policy_id?: string;
}

export interface PolicyPreviewRepository {
  kind: "github" | "azure_devops" | "other";
  repos: string[];
  org?: string;
  default_profile: string[];
  capability_ceiling: string[];
}

/** Who is responsible for a provenance row; an id or name only where the kind has one. */
export interface ProvenanceSource {
  // An unknown kind renders no chip, never a guess.
  kind: "policy" | "ceiling" | "workspace" | "component" | "model_provider" | "person" | (string & {});
  id?: string;
  name?: string;
}

/**
 * Why one entry of the resolved spec is there (internal/api's provenanceRow).
 * field is a RunPolicySpec json name; value a redaction-safe entry key: a host,
 * a grant as "kind:host", a mount's target, a repository as "repo@ref", a capability.
 */
export interface ProvenanceEntry {
  field: string;
  value: string;
  source: ProvenanceSource;
  // clamped: the source asked and the ceiling removed it. narrowed: it stands, reduced, or a
  // capability was taken away. removed: the person's override dropped it.
  effect: "added" | "clamped" | "narrowed" | "removed" | (string & {});
}

/**
 * What a member of the caller's type would be held to, for a caller whose own runs stand
 * outside governance (pkg/client's UngovernedNotice). At most one of no_person, no_unit and
 * unavailable is set; with none set, unit names the unit and limits its headline limits.
 */
export interface UngovernedNotice {
  no_person?: boolean;
  no_unit?: boolean;
  unavailable?: boolean;
  unit?: string;
  /** At most three short phrases, in a fixed order. */
  limits?: string[];
  /** The unit sets further limits than `limits` lists. */
  more?: boolean;
}

/** Authorized, provisional read output. Never use it as the authored draft. */
export interface PolicyPreviewResult {
  spec: RunPolicySpec;
  source: PolicyPreviewSource;
  provisional: boolean;
  redacted: boolean;
  warnings: string[];
  pending: PolicyPreviewPending[];
  repository_access: PolicyPreviewRepository[];
  // The run-level reason the per-person Azure DevOps credential would not be
  // given to this run's sandbox; absent when no structural decline was reported.
  ado_lane_declined?: string;
  // The draft refuses every push and carries an environment or file secret of its own, so a push token added as a
  // secret could still reach a code host Wardyn does not recognize. Set by the forge guard.
  push_deny_all_secrets?: boolean;
  // The install's git token broker is off and the run's capability list leaves out code_write, so a git token in
  // the sandbox can still push. Set from the decision behind the run's unstoppable-push warning, which the preview
  // does not send as a warning.
  pushes_unstoppable?: boolean;
  // Absent for a draft with no component and no GitHub or Azure DevOps repository.
  components?: ComponentFact[];
  // Why each entry is in `spec`, from the same fold as the launch. The server always
  // sends the array; optional here so an older server's body still types.
  provenance?: ProvenanceEntry[];
  // The preflight's limits and applied size, with the same meaning.
  limits?: EffectiveRunLimits;
  resources_applied?: AppliedResources;
  // The main-drive size it asked for and gets, the preflight's.
  main_drive?: MainDriveApplied;
  // Whether each entry can run on the person's runner; the server always sends
  // the array, empty until its lane fills it (optional here so an older server still types).
  local_placement?: LocalPlacementFact[];
  // The image the run would start from and its source (absent until the image
  // lane resolves it), and the images the organisation lets the person choose.
  image?: ImageFact;
  // "checked at launch" when launch checks the image's signatures; absent when it checks nothing.
  image_admission?: ImageAdmissionFact;
  allowed_images?: AllowedImage[];
  // The preflight's harness_set fact, with the same meaning.
  harness_set?: HarnessSetFact;
  // The preflight's allowed_models and allowed_models_unit, with the same meaning.
  allowed_models?: string[];
  allowed_models_unit?: string;
  // The preflight's pool facts, with the same meaning.
  runner_pool?: ResolvedRunnerPool;
  runner_pools?: RunnerPoolChoice[];
  // The preflight's runner_pool_slots, with the same meaning.
  runner_pool_slots?: PoolSlots;
  // What the draft brings in, Review's same block (optional so an older server still types).
  access?: AccessFacts;
  // The run's mount map: each repository, folder and drive at the path the run
  // uses it at, the one holding the starting folder marked. The server always
  // sends the array; optional here so an older server still types.
  attachments?: RunAttachment[];
  // Present only when the caller's own runs stand outside governance; absent otherwise.
  ungoverned?: UngovernedNotice;
}
