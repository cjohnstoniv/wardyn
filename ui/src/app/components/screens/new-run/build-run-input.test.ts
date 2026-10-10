/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { runWireBody } from "../../../lib/api/runs";
import { makeWorkspace } from "../../../../test/factories";
import { buildRunInput, type RunInputParams } from "./build-run-input";
import { initialWizardState } from "./wizard-types";
import { mergeRunSelections } from "./wizard-spec";

const workspaces = [
  makeWorkspace({ id: "local", kind: "local_dir", source: "/data/local" }),
  makeWorkspace({ id: "repo", kind: "repo", source: "team/repo" }),
  makeWorkspace({ id: "multi", kind: "", source: "", sources: [
    { type: "local_dir", path: "/data/multi" },
    { type: "repo", source: "team/other" },
  ] }),
  makeWorkspace({ id: "scratch", kind: "ephemeral", source: "", sources: [
    { type: "ephemeral", target: "/scratch" },
  ] }),
];

function input(over: Partial<RunInputParams> = {}): RunInputParams {
  const state = over.state ?? initialWizardState("CC2", { selectedPolicyId: "saved-id" });
  return {
    state,
    workspaces,
    policyMode: "saved",
    ccTouched: false,
    merged: mergeRunSelections({ allowed_domains: ["authored.example"], first_use_approval: "deny_with_review", min_confinement_class: "CC1" }, state, workspaces),
    ...over,
  };
}

describe("buildRunInput — reference policy workspace refusal", () => {
  it.each(["default", "saved"] as const)("%s refuses every multiple-attachment selection without modifying it", (policyMode) => {
    for (const ids of [["local", "repo"], ["local", "repo", "multi"], ["local", "unresolved"]]) {
      const state = initialWizardState("CC2", {
        selectedPolicyId: "saved-id",
        workspaces: ids.map((workspaceId) => ({ workspaceId })),
      });
      const before = structuredClone(state);
      expect(buildRunInput(input({ policyMode, state }))).toBeNull();
      expect(state).toEqual(before);
    }
  });

  it.each(["default", "saved"] as const)("%s still represents zero or one workspace without an inline document", (policyMode) => {
    for (const workspaceId of [undefined, "local", "repo", "multi", "scratch"]) {
      const state = initialWizardState("CC2", {
        selectedPolicyId: "saved-id",
        workspaces: workspaceId ? [{ workspaceId }] : [],
      });
      const body = buildRunInput(input({ policyMode, state, merged: null }));
      expect(body).not.toBeNull();
      expect(body?.workspace_id).toBe(workspaceId);
      expect(body).not.toHaveProperty("inline_policy");
      expect(body?.policy_id).toBe(policyMode === "saved" ? "saved-id" : undefined);
      expect(JSON.parse(JSON.stringify(body))).not.toHaveProperty("confinement_class");
    }
  });

  it("saved mode without a selected policy never falls back to an inline document", () => {
    expect(buildRunInput(input({ state: initialWizardState() }))).toBeNull();
  });

  it("an explicit confinement choice is retained", () => {
    expect(buildRunInput(input({ ccTouched: true }))?.confinement_class).toBe("CC2");
  });
});

describe("buildRunInput — custom policy", () => {
  it("retains all selected sources and options despite a dormant saved policy ID", () => {
    const state = initialWizardState("CC2", {
      selectedPolicyId: "saved-id",
      workspaces: [
        { workspaceId: "local", readOnly: true },
        { workspaceId: "repo", enabledOptional: ["read:team/repo"] },
        { workspaceId: "multi" },
      ],
    });
    const before = structuredClone(state);
    const body = buildRunInput(input({ policyMode: "custom", state }));
    expect(body).not.toHaveProperty("policy_id");
    expect(body?.inline_policy?.allowed_domains).toContain("authored.example");
    expect(body?.inline_policy?.workspace_mounts).toMatchObject([
      { source: "/data/local", read_only: true },
      { source: "/data/multi" },
    ]);
    expect(body?.inline_policy?.workspace_repos).toEqual([{ repo: "team/repo" }, { repo: "team/other" }]);
    expect(body?.workspaces).toEqual([
      { workspace_id: "local", read_only: true },
      { workspace_id: "repo", enabled_optional: ["read:team/repo"] },
    ]);
    expect(state).toEqual(before);
  });

  it("never substitutes a saved reference or a composed fallback for an invalid document", () => {
    expect(buildRunInput(input({ policyMode: "custom", merged: null }))).toBeNull();
  });
});

describe("buildRunInput — components", () => {
  const ref = [{ id: "6f0c1d2e-0000-4000-8000-0000000c0301" }, { inline: { hosts: ["api.example"] }, name: "Tool" }];

  it.each(["default", "saved", "custom"] as const)("%s mode carries the run's components", (policyMode) => {
    const state = initialWizardState("CC2", { selectedPolicyId: "saved-id", components: ref });
    expect(buildRunInput(input({ policyMode, state }))?.components).toEqual(ref);
  });

  it("sends no components key when the run carries none", () => {
    for (const policyMode of ["default", "saved", "custom"] as const) {
      expect(buildRunInput(input({ policyMode }))).not.toHaveProperty("components");
    }
  });

  it("reaches the wire body all three doors send", () => {
    const state = initialWizardState("CC2", { selectedPolicyId: "saved-id", components: ref });
    const body = runWireBody(buildRunInput(input({ state }))!);
    expect(body.components).toEqual(ref);
  });
});

describe("buildRunInput — the 0.9 contract fields", () => {
  it("forwards an explicit empty ADO edit only while active and keeps it for reactivation", () => {
    const state = {
      ...initialWizardState("CC1"),
      title: "test",
      task: "test",
      contract: { runner: {}, access: { overrides: { azureDevOps: { capabilities: [] }, gitPAT: [], pushRules: [] } } },
    };
    const sections = { agent: false, azureDevOps: true, gitPATHosts: [], pushKeys: [] };
    const activeInput = buildRunInput({ ...defaultInput(state), activeSections: sections })!;
    expect(runWireBody(activeInput).overrides).toEqual({ azure_devops: { capabilities: [] } });
    const inactiveInput = buildRunInput({ ...defaultInput(state), activeSections: { ...sections, azureDevOps: false } })!;
    expect(runWireBody(inactiveInput).overrides).toBeUndefined();
    expect(state.contract.access.overrides.azureDevOps).toEqual({ capabilities: [] });
    const restoredInput = buildRunInput({ ...defaultInput(state), activeSections: sections })!;
    expect(runWireBody(restoredInput).overrides).toEqual({ azure_devops: { capabilities: [] } });
    const untouched = { ...state, contract: { ...state.contract, access: { overrides: { gitPAT: [], pushRules: [] } } } };
    expect(runWireBody(buildRunInput({ ...defaultInput(untouched), activeSections: sections })!).overrides).toBeUndefined();
  });

  const defaultInput = (state = initialWizardState("CC2")) => input({ policyMode: "default", state, merged: null });

  it("sends none of them for an untouched form", () => {
    const body = buildRunInput(defaultInput());
    for (const key of ["placement", "runner_id", "resources", "overrides"]) expect(body).not.toHaveProperty(key);
  });

  it("sends the person's choices, and the overrides of active sections only", () => {
    const state = initialWizardState("CC2", {
      contract: {
        runner: { placement: "local", runnerId: "runner-1", cpus: 2 },
        access: {
          overrides: {
            gitPAT: [],
            pushRules: [{ provider: "github", org: "acme", deny_paths: ["infra/"] }],
            azureDevOps: { capabilities: ["code_read"] },
          },
        },
      },
    });
    const inactive = runWireBody(buildRunInput(defaultInput(state))!);
    expect(inactive).toMatchObject({ placement: "local", runner_id: "runner-1", resources: { cpu_millis: 2000 } });
    expect(inactive).not.toHaveProperty("overrides");
    const active = runWireBody(
      buildRunInput({ ...defaultInput(state), activeSections: { agent: false, azureDevOps: false, gitPATHosts: [], pushKeys: ["github/acme"] } })!,
    );
    expect(active.overrides).toEqual({ push_rules: [{ provider: "github", org: "acme", deny_paths: ["infra/"] }] });
  });
});
