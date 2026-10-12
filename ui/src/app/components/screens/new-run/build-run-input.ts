/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunPolicySpec, SetupModelProvider, Workspace } from "../../../lib/types";
import { buildRunContractWire, NO_ACTIVE_SECTIONS, type ActiveSections, type RunContractWire } from "../../../lib/run-contract-draft";
import { LEGACY_RUN_MODE_FIELDS, mapRunMode } from "../../../lib/run-mode-map";
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

/**
 * The wizard's run shape as the 0.9 contract's carriers, with the older fields they replace
 * removed. It reads the FINAL body, policy and overrides included, because the server refuses
 * some of those beside carriers. A shape the contract cannot say, or a draft that already chose
 * its experience, keeps the older fields untouched.
 */
function withRunMode<T extends CreateRunInputWithComposition & RunContractWire>(body: T): T {
  const carriers = body.experience ? null : mapRunMode(body);
  if (!carriers) return body;
  const rest: CreateRunInputWithComposition & RunContractWire = { ...body };
  for (const field of LEGACY_RUN_MODE_FIELDS) delete rest[field];
  return { ...rest, ...carriers } as unknown as T;
}

/** Returns no request when the active policy mode cannot represent the selections. */
export function buildRunInput({
  state, workspaces, modelProviders, policyMode, ccTouched, merged, activeSections = NO_ACTIVE_SECTIONS,
}: RunInputParams): (CreateRunInputWithComposition & { inline_policy?: RunPolicySpec; mode_contract?: "0.9" }) | null {
  if (referenceLaneDropsWorkspace(policyMode, state.workspaces.length)) return null;
  if (policyMode === "saved" && !state.selectedPolicyId) return null;

  const { run: built } = buildSpec(state, workspaces, modelProviders);
  const sized = ccTouched ? built : { ...built, confinement_class: undefined };
  // Same body on all three doors; none when the run carries none.
  const withComponents = state.components.length ? { ...sized, components: state.components } : sized;
  const contract = buildRunContractWire(state.contract, activeSections);
  const run = { ...withComponents, ...contract };
  // A saved ID may survive a mode switch; only the active mode chooses the policy.
  if (policyMode === "custom") return merged ? withRunMode({ ...run, inline_policy: merged.spec }) : null;

  // Preserve buildSpec's ephemeral fallback when there is no mount/repo primary.
  const referenced = { ...run, workspace_id: primaryWorkspaceId(state.workspaces, workspaces) ?? run.workspace_id };
  return withRunMode(policyMode === "saved" ? { ...referenced, policy_id: state.selectedPolicyId } : referenced);
}
