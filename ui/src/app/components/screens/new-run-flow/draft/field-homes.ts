/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where each run-document field is changed, and which Review group shows it.
// Keys are the template field registry's request names and every policy name
// as `inline_policy.<name>`; a registry row without a home fails its test.

import type { NewRunReason, PlacementReason, WorkspaceDoorReason } from "../../../../lib/new-run-refusals";
import type { RunModeReason } from "../../../../lib/run-mode-refusals";
import type { RunnerPoolReason } from "../../../../lib/runner-pool-refusals";
import type { TemplateReason } from "../../../../lib/template-refusals";
import type { NewRunStep } from "../lenses/step-issues";

export type FieldHome = NewRunStep | "review" | "document" | "excluded";
export type ReviewGroupId = "run" | "where" | "work" | "tools" | "access" | "limits";

const homes = (home: FieldHome, names: string): Record<string, FieldHome> =>
  Object.fromEntries(names.split(" ").map((n) => [n, home]));
const groups = (group: ReviewGroupId, names: string): Record<string, ReviewGroupId> =>
  Object.fromEntries(names.split(" ").map((n) => [n, group]));
const policy = (names: string) => names.split(" ").map((n) => `inline_policy.${n}`).join(" ");

const POLICY_REVIEW = "allowed_domains denied_domains allow_all_egress first_use_approval first_use_hold_seconds max_holds " +
  "allowed_methods eligible_grants tool_rules git_push_any_branch push_rules azure_devops_capabilities github_capabilities";
const POLICY_DOCUMENT = "auto_stop_after_sec ui_apps llm_inspection min_confinement_class workspace_mounts workspace_repos resources";

/**
 * Policy rows go to Review, never to Tools and access: that step writes only
 * overrides and components, so a policy value is shown as set by the policy.
 */
export const FIELD_HOME: Record<string, FieldHome> = {
  ...homes("shape", "shape"),
  ...homes("run", "experience workload runner_pool_id confinement_class"),
  ...homes("harnesses", "tools startup"),
  ...homes("files", "workspaces drive drives start_folder no_repositories_or_drives"),
  ...homes("tools", "components overrides"),
  ...homes("review", "resources inline_policy"),
  ...homes("document", "ends_after_sec no_end image allowed_image devcontainer_repo devcontainer_ref placement runner_id " +
    "policy_id preset preset_version agent task repo workspace_id interactive task_mode interactive_start seed_auto_tools " +
    "tool_approvals integration_id model_provider"),
  ...homes("excluded", "title description mode_contract max_wait_sec policy_overlay"),
  ...homes("review", policy(POLICY_REVIEW)),
  ...homes("document", policy(POLICY_DOCUMENT)),
};

/** Known subpaths whose home differs from their top-level field's. Array indices are written `[]`. */
export const FIELD_HOME_OVERRIDES: Record<string, FieldHome> = {
  "workload.agent": "harnesses",
  "workload.task": "harnesses",
  "workload.command": "run",
  "resources.disk_mib": "files",
  "tools[].model_provider": "tools",
  "tools[].tool_rules": "tools",
  "tools[].default_effect": "tools",
};

export const REVIEW_GROUP_OF_FIELD: Record<string, ReviewGroupId> = {
  ...groups("run", "shape experience workload interactive task_mode interactive_start agent task preset preset_version title description mode_contract max_wait_sec policy_overlay"),
  ...groups("where", "runner_pool_id confinement_class placement runner_id image allowed_image devcontainer_repo devcontainer_ref " +
    policy("min_confinement_class")),
  ...groups("work", "workspaces drive drives start_folder no_repositories_or_drives repo workspace_id " + policy("workspace_mounts workspace_repos")),
  ...groups("tools", "tools startup components model_provider integration_id seed_auto_tools tool_approvals " + policy("ui_apps")),
  ...groups("access", "overrides policy_id inline_policy " + policy(`${POLICY_REVIEW} llm_inspection`)),
  ...groups("limits", "resources ends_after_sec no_end " + policy("resources auto_stop_after_sec")),
};

/** Every reason a preflight or create refusal can carry that the console names. */
export type KnownLaunchReason = NewRunReason | PlacementReason | RunModeReason | RunnerPoolReason | TemplateReason | WorkspaceDoorReason;

/** Written out key by key, so a new reason without a home fails typecheck. */
export const HOME_OF_REASON: Record<KnownLaunchReason, FieldHome> = {
  placement_invalid: "document", image_conflict: "document", allowed_image_invalid: "document",
  workspace_target_invalid: "files", workspace_target_overlap: "files", workspace_pin_conflict: "files",
  workspace_selection_invalid: "files", workspace_repo_unknown: "files", run_ref_invalid: "files",
  workspace_ado_org_conflict: "files",
  workspace_repo_not_admitted: "files", repo_url_credentials: "files", capability_workspace: "files", capability_workspace_provider: "files",
  resources_invalid: "review", request_field_unavailable: "review",
  override_invalid: "tools", override_refused: "tools",
  placement_unavailable: "run", placement_denied: "run", placement_trusted_output: "run",
  runner_offline: "run", runner_at_capacity: "run", runner_limits_unset: "run", runner_ambiguous: "run", runner_not_found: "run", runner_unclaimed: "run", runner_revoked: "run",
  runner_token_invalid: "run", runner_claim_mismatch: "run", runner_posture_unmet: "run", runner_keystore_unavailable: "run", runner_recording_off: "run",
  recording_store_off: "run",
  runner_action_pending: "run", relay_runner_mismatch: "run", relay_required: "run", relay_route_refused: "run",
  placement_credential: "run", placement_component_self_defined: "tools", placement_capability: "tools",
  delivery_via_org: "tools", delivery_runner_resident: "tools", delivery_not_via_org: "tools",
  via_org_destination_refused: "tools", via_org_cap_exceeded: "tools",
  placement_local_path: "files",
  run_mode_required: "run", run_mode_invalid: "run", run_mode_conflict: "run", run_background_only: "run",
  start_folder_invalid: "files", main_drive_unenforceable: "files",
  run_shape_conflict: "shape",
  runner_pool_invalid: "run", runner_pool_required: "run", runner_pool_not_found: "run", runner_pool_unavailable: "run",
  runner_pool_default_unavailable: "run", runner_pool_stale: "run", runner_pool_no_eligible_member: "run",
  runner_pool_member_mismatch: "run", runner_pools_unavailable: "run", runner_pool_run_type_not_allowed: "run", runner_pool_run_type_missing: "run",
  runner_pool_barrier_not_allowed: "run", runner_pool_queue_full: "run", runner_pool_full: "run",
  template_document_invalid: "document", template_version_unsupported: "document", template_field_unknown: "document",
  template_field_invalid: "document", template_field_excluded: "document", template_secret_refused: "document",
  template_run_state_refused: "document", template_metadata_in_content: "document", template_field_unavailable: "document",
  template_coverage_invalid: "document",
  template_dependency_missing: "tools",
  template_shared_field_refused: "review", template_scope_forbidden: "review", template_group_unverified: "review",
  template_not_found: "review", template_revision_conflict: "review", templates_unavailable: "review",
  template_name_taken: "review", template_limit_reached: "review", template_policy_field_refused: "review",
  harness_not_installed: "harnesses", harness_set_no_image: "harnesses", harness_set_custom_image: "harnesses",
  harness_providers_conflict: "harnesses",
};

/** The one field a refusal names, where it names one: the run's runner, chosen through its pool. */
export const FIELD_OF_REASON: Partial<Record<KnownLaunchReason, string>> = {
  placement_credential: "runner_pool_id",
};

/** A document path's Review group: its top-level field's (or `inline_policy.<name>`). */
export function groupOfPath(path: string): ReviewGroupId | undefined {
  const [top, policyField] = path.replace(/\[\d*\]/g, "").split(".");
  return (top === "inline_policy" && policyField && REVIEW_GROUP_OF_FIELD[`inline_policy.${policyField}`]) || REVIEW_GROUP_OF_FIELD[top];
}

/** A refusal's step; a reason the console does not know goes to Review. */
export const homeOfReason = (reason: string): FieldHome =>
  (HOME_OF_REASON as Record<string, FieldHome>)[reason] ?? "review";

/** A document path's home: the longest known subpath, else its top-level field (or `inline_policy.<name>`). */
export function homeOfPath(path: string): FieldHome | undefined {
  const parts = path.replace(/\[\d+\]|\.\d+(?=\.|$)/g, "[]").split(".");
  for (let n = parts.length; n > 1; n--) {
    const home = FIELD_HOME_OVERRIDES[parts.slice(0, n).join(".")];
    if (home) return home;
  }
  const [top, policyField] = parts.map((p) => p.replace(/\[\]$/, ""));
  return top === "inline_policy" && policyField ? FIELD_HOME[`inline_policy.${policyField}`] : FIELD_HOME[top];
}
