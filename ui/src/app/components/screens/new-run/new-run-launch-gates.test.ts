/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { RUN, RAIL_PROVIDER, RAIL_SETUP } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { POLICY_TEMPLATE_COPY } from "../../wardyn/copy/policy-templates";
import { DENIED } from "../../../lib/permissions-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import type { Workspace } from "../../../lib/types";
import {
  ISSUE_TARGET,
  issueCounts,
  launchGates,
  preflightHolds,
  shownIssue,
  withPreflightIssues,
  type LaunchGateInputs,
} from "./new-run-launch-gates";

function gates(over: Partial<LaunchGateInputs> = {}) {
  return launchGates({
    isAgent: true,
    mode: "interactive",
    task: "",
    title: "A run",
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

const texts = (over: Partial<LaunchGateInputs> = {}) => gates(over).issues.map((i) => i.text);

describe("New Run local reference workspace gate", () => {
  it.each(["default", "saved", "custom"] as const)("%s exposes the attachment gate independently of its visible sentence", (policyMode) => {
    for (const attachedWorkspaces of [0, 1, 2, 4]) {
      const result = gates({ policyMode, attachedWorkspaces });
      expect(result.referenceWorkspaceBlocked).toBe(policyMode !== "custom" && attachedWorkspaces > 1);
      expect(result.defaultWorkspaceProblem).toBe(policyMode === "default" && attachedWorkspaces > 1
        ? POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE
        : null);
    }
  });

  it.each(["default", "saved"] as const)("%s retains earlier task validation while recording the attachment refusal", (policyMode) => {
    const result = gates({ policyMode, mode: "batch" });
    expect(result.issues[0].text).toBe("An autonomous run needs a task to perform.");
    expect(result.referenceWorkspaceBlocked).toBe(true);
  });

  it("a saved policy known to be gone retains its earlier problem", () => {
    const result = gates({ savedPolicy: undefined });
    expect(result.issues[0].text).toBe(RUN.POLICY_GONE);
    expect(result.referenceWorkspaceBlocked).toBe(true);
  });

  it("a saved policy still loading is not declared gone", () => {
    expect(texts({ savedPolicy: undefined, policiesLoaded: false })).not.toContain(RUN.POLICY_GONE);
  });

  it("the missing saved selection keeps its own problem", () => {
    const result = gates({ savedPolicy: undefined, selectedPolicyId: undefined });
    expect(result.issues[0].text).toBe("Pick a saved policy, or write a custom one.");
    expect(result.referenceWorkspaceBlocked).toBe(true);
  });
});

// #1922: anything that holds Launch is an issue naming its panel and control.
describe("New Run launch issues", () => {
  const custom = { policyMode: "custom", attachedWorkspaces: 0 } as const;
  const ws = (over: Partial<Workspace>): Workspace =>
    ({ id: "ws1", name: "repo-a", kind: "repo", source: "https://github.com/acme/a", status: "scanned", ...over }) as Workspace;

  it("a form with nothing against it has no issues", () => {
    expect(gates(custom).issues).toEqual([]);
  });

  it("an empty required Title is an issue on Run, after the task it may be derived from", () => {
    expect(gates({ ...custom, title: "  " }).issues).toEqual([
      { panel: "run", focus: ISSUE_TARGET.TITLE, text: NEW_RUN_FLOW.TITLE_REQUIRED },
    ]);
    const both = gates({ ...custom, mode: "batch", title: "" }).issues;
    expect(both.map((i) => i.text)).toEqual(["An autonomous run needs a task to perform.", NEW_RUN_FLOW.TITLE_REQUIRED]);
    expect(issueCounts(both)).toEqual({ run: 2, workspace: 0, access: 0, policy: 0 });
  });

  it("a shell command asks for a command, in its own words", () => {
    expect(texts({ ...custom, isAgent: false })).toEqual(["Enter a command to run."]);
  });

  it("an unparseable custom policy is an issue on Policy, at the source", () => {
    expect(gates({ ...custom, specParsedOk: false }).issues).toEqual([
      { panel: "policy", focus: ISSUE_TARGET.POLICY_SOURCE, text: "The policy spec isn't valid JSON." },
    ]);
  });

  it("a workspace that is not available, or whose git provider is not enabled, is an issue on Workspace", () => {
    const unavailable = gates({ ...custom, workspaces: [ws({ available_to_you: false })], selectedWorkspaceId: "ws1" });
    expect(unavailable.issues).toEqual([
      { panel: "workspace", focus: ISSUE_TARGET.WORKSPACE, text: DENIED.WORKSPACE_NOT_AVAILABLE, inline: true },
    ]);
    expect(unavailable.workspaceUnavailable).toBe(true);

    const notAdmitted = gates({
      ...custom,
      workspaces: [ws({ available_to_you: true, sources: [{ type: "repo", source: "https://git.example/a", admitted: false }] })],
      selectedWorkspaceId: "ws1",
    });
    expect(notAdmitted.issues.map((i) => [i.panel, i.text])).toEqual([["workspace", PROVIDERS.CARD_NOT_ADMITTED]]);
  });

  it("the default lane's second workspace is an issue on Policy", () => {
    expect(gates({ policyMode: "default" }).issues).toEqual([
      { panel: "policy", focus: ISSUE_TARGET.POLICY_MODE, text: POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE, inline: true },
    ]);
  });

  it("several candidates and no pick wait for one, unless a workspace pin answers", () => {
    const two = [{ id: "a" }, { id: "b" }] as LaunchGateInputs["providerCandidates"];
    expect(gates({ ...custom, providerCandidates: two }).issues).toEqual([
      { panel: "run", focus: ISSUE_TARGET.PROVIDER_PICKER, text: RAIL_PROVIDER.LAUNCH_HINT },
    ]);
    expect(gates({ ...custom, providerCandidates: two, pin: "a" }).issues).toEqual([]);
    expect(gates({ ...custom, providerCandidates: two, isAgent: false, task: "make test" }).issues).toEqual([]);
  });

  it("an issue its own panel prints beside the control is not named above Launch there", () => {
    const inline = gates({ policyMode: "default" }).issues;
    expect(shownIssue(inline, "policy")).toBeNull();
    expect(shownIssue(inline, "run")).toBe(inline[0]);
    const title = gates({ ...custom, title: "" }).issues;
    expect(shownIssue(title, "run")).toBe(title[0]);
    expect(shownIssue([], "run")).toBeNull();
  });
});

describe("New Run preflight holds", () => {
  const base = { isAgent: true, isInteractive: false, agentName: "Claude Code", noBarrier: false, preflightFresh: true };
  const row = (kind: string, status: string) => [{ kind, status }];

  it("only a fresh, missing row holds", () => {
    expect(preflightHolds({ ...base, setupItems: row("backend", "missing") })).toEqual({ backendMissing: true, modelBlocked: false });
    expect(preflightHolds({ ...base, setupItems: row("backend", "unverified") }).backendMissing).toBe(false);
    expect(preflightHolds({ ...base, preflightFresh: false, setupItems: row("backend", "missing") }).backendMissing).toBe(false);
    expect(preflightHolds({ ...base, noBarrier: true, setupItems: row("backend", "missing") }).backendMissing).toBe(false);
    expect(preflightHolds({ ...base, runnerUnknown: true, setupItems: row("backend", "missing") }).backendMissing).toBe(false);
  });

  it("a missing model holds an unattended agent run only", () => {
    expect(preflightHolds({ ...base, setupItems: row("llm_access", "missing") }).modelBlocked).toBe(true);
    expect(preflightHolds({ ...base, isInteractive: true, setupItems: row("llm_access", "missing") }).modelBlocked).toBe(false);
    expect(preflightHolds({ ...base, isAgent: false, setupItems: row("llm_access", "missing") }).modelBlocked).toBe(false);
  });

  it("the barrier outranks the form's own issues, and the model block speaks last", () => {
    const local = [{ panel: "run" as const, focus: ISSUE_TARGET.TITLE, text: NEW_RUN_FLOW.TITLE_REQUIRED }];
    expect(withPreflightIssues(local, { backendMissing: true, modelBlocked: true }, "Claude Code").map((i) => [i.panel, i.text])).toEqual([
      ["policy", RAIL_SETUP.BACKEND_BLOCK],
      ["run", NEW_RUN_FLOW.TITLE_REQUIRED],
      ["run", RAIL_MODEL_ACCESS.UNATTENDED_BLOCK("Claude Code")],
    ]);
  });
});
