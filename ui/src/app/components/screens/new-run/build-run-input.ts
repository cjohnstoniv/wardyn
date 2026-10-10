/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunPolicySpec, SetupModelProvider, Workspace } from "../../../lib/types";
import { buildRunContractWire, NO_ACTIVE_SECTIONS, type ActiveSections } from "../../../lib/run-contract-draft";
import type { PolicyMode } from "../../wardyn/policy-panel";
import { referenceLaneDropsWorkspace } from "./new-run-launch-gates";
import { buildSpec, mergeRunSelections } from "./wizard-spec";
import { primaryWorkspaceId, type CreateRunInputWithComposition, type WizardState } from "./wizard-types";

export interface RunInputParams {
  state: WizardState;
  workspaces: Workspace[];
  modelProviders?: SetupModelProvider[];
  policyMode: PolicyMode;
  /** Untouched Barrier controls leave the server's default in charge. */
  ccTouched: boolean;
  /** The authored policy with run selections, or null while it cannot parse. */
  merged: ReturnType<typeof mergeRunSelections> | null;
  /** The Access sections on the page; overrides of any other section stay in the draft and are not sent. */
  activeSections?: ActiveSections;
}

/** Returns no request when the active policy mode cannot represent the selections. */
export function buildRunInput({
  state, workspaces, modelProviders, policyMode, ccTouched, merged, activeSections = NO_ACTIVE_SECTIONS,
}: RunInputParams): (CreateRunInputWithComposition & { inline_policy?: RunPolicySpec }) | null {
  if (referenceLaneDropsWorkspace(policyMode, state.workspaces.length)) return null;
  if (policyMode === "saved" && !state.selectedPolicyId) return null;

  const { run: built } = buildSpec(state, workspaces, modelProviders);
  const sized = ccTouched ? built : { ...built, confinement_class: undefined };
  // Same body on all three doors; none when the run carries none.
  const withComponents = state.components.length ? { ...sized, components: state.components } : sized;
  const run = { ...withComponents, ...buildRunContractWire(state.contract, activeSections) };
  // A saved ID may survive a mode switch; only the active mode chooses the policy.
  if (policyMode === "custom") return merged ? { ...run, inline_policy: merged.spec } : null;

  // Preserve buildSpec's ephemeral fallback when there is no mount/repo primary.
  const referenced = { ...run, workspace_id: primaryWorkspaceId(state.workspaces, workspaces) ?? run.workspace_id };
  return policyMode === "saved" ? { ...referenced, policy_id: state.selectedPolicyId } : referenced;
}
