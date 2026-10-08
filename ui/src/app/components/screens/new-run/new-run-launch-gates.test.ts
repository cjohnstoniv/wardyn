/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { RUN } from "../../wardyn/copy";
import { POLICY_TEMPLATE_COPY } from "../../wardyn/copy/policy-templates";
import { launchGates, type LaunchGateInputs } from "./new-run-launch-gates";

function gates(over: Partial<LaunchGateInputs> = {}) {
  return launchGates({
    isAgent: true,
    mode: "interactive",
    task: "",
    policyMode: "saved",
    specParsedOk: true,
    selectedPolicyId: "policy-id",
    savedPolicy: { id: "policy-id", name: "Policy", spec: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" } },
    policiesLoaded: true,
    pin: undefined,
    workspaces: [],
    selectedWorkspaceId: undefined,
    attachedWorkspaces: 2,
    caps: null,
    modelProviders: undefined,
    providerGateState: undefined,
    providerCandidates: [],
    selectedModelProviderId: undefined,
    agentName: "Claude Code",
    ...over,
  });
}

describe("New Run local reference workspace gate", () => {
  it.each(["default", "saved", "custom"] as const)("%s exposes the attachment gate and its mode sentence", (policyMode) => {
    const sentence = {
      default: "The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.",
      saved: "A saved policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.",
      custom: null,
    }[policyMode];
    for (const attachedWorkspaces of [0, 1, 2, 4]) {
      const result = gates({ policyMode, attachedWorkspaces });
      expect(result.referenceWorkspaceBlocked).toBe(policyMode !== "custom" && attachedWorkspaces > 1);
      expect(result.referenceWorkspaceProblem).toBe(attachedWorkspaces > 1 ? sentence : null);
    }
    expect(POLICY_TEMPLATE_COPY.SAVED_ONE_WORKSPACE).toBe(
      "A saved policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.",
    );
  });

  it.each(["default", "saved"] as const)("%s alone is not the rail's problem sentence: the panel says it once", (policyMode) => {
    const result = gates({ policyMode, mode: "batch", task: "do it" });
    expect(result.problem).toBeNull();
    expect(result.referenceWorkspaceBlocked).toBe(true);
    expect(result.referenceWorkspaceProblem).not.toBeNull();
  });

  it.each(["default", "saved"] as const)("%s retains earlier task validation while recording the attachment refusal", (policyMode) => {
    const result = gates({ policyMode, mode: "batch" });
    expect(result.problem).toBe("An autonomous run needs a task to perform.");
    expect(result.referenceWorkspaceBlocked).toBe(true);
    expect(result.referenceWorkspaceProblem).not.toBeNull();
  });

  it("a saved policy known to be gone retains its earlier problem", () => {
    const result = gates({ savedPolicy: undefined });
    expect(result.problem).toBe(RUN.POLICY_GONE);
    expect(result.referenceWorkspaceBlocked).toBe(true);
  });

  it("a saved policy still loading is not declared gone", () => {
    expect(gates({ savedPolicy: undefined, policiesLoaded: false }).problem).not.toBe(RUN.POLICY_GONE);
  });

  it("the missing saved selection keeps its own problem", () => {
    const result = gates({ savedPolicy: undefined, selectedPolicyId: undefined });
    expect(result.problem).toBe("Pick a saved policy, or write a custom one.");
    expect(result.referenceWorkspaceBlocked).toBe(true);
  });
});
