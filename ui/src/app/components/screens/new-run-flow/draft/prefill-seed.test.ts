/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { addHarness } from "../lenses/harness-lens";
import { prefillSeed } from "./prefill-seed";

const clone = (state: object) => prefillSeed({ prefill: { inlinePolicy: false, state } });

describe("router-state prefill seeds", () => {
  it("reads nothing from state with no prefill", () => {
    expect(prefillSeed(null)).toBeUndefined();
    expect(prefillSeed({ created: {} })).toBeUndefined();
  });

  it("makes the composer's text an agent task and keeps its workspace", () => {
    expect(prefillSeed({ prefill: { source: "composer", state: { task: "fix it", workspaces: [{ workspaceId: "w1" }] } } })).toEqual({
      origin: { kind: "composer" },
      intent: { shape: "agent_task", workload: { kind: "agent_task", task: "fix it" }, workspaces: [{ workspace_id: "w1" }] },
    });
  });

  it("clones each run shape into the 0.9 carriers, without title or description", () => {
    const common = { title: "Old", description: "why", confinementClass: "CC3", selectedPolicyId: "p1", workspaces: [{ workspaceId: "w1" }] };
    const kept = { confinement_class: "CC3", policy_id: "p1", workspaces: [{ workspace_id: "w1" }] };
    expect(clone({ ...common, runType: "command", command: "make test" })).toEqual({
      origin: { kind: "clone" },
      intent: { ...kept, shape: "application", experience: "background", workload: { kind: "command", command: "make test" } },
    });
    expect(clone({ ...common, runType: "agent", mode: "interactive", agent: "codex-cli", interactiveStart: "agent" })?.intent).toEqual({
      ...kept, shape: "agent_task", experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }],
      startup: { kind: "harness", tool: "codex-cli" },
    });
    expect(clone({ runType: "agent", mode: "interactive", agent: "claude-code", interactiveStart: "shell", startupCommand: "make dev" })?.intent).toEqual({
      shape: "environment", experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }],
      startup: { kind: "command", command: "make dev" },
    });
  });

  it("keeps a held posture with no harness it can name, so any harness added later starts held", () => {
    const seed = clone({ runType: "agent", mode: "batch", toolApprovals: "hold", task: "t" });
    expect(seed?.intent).toMatchObject({ tool_approvals: "hold" });
    expect(seed?.intent.tools).toBeUndefined();
    expect(addHarness(seed?.intent ?? {}, "claude-code").tools).toEqual([{ id: "claude-code", kind: "harness", default_effect: "hold" }]);
    expect(addHarness({}, "claude-code").tools).toEqual([{ id: "claude-code", kind: "harness" }]);
  });

  it("marks a clone whose source policy was inline and not kept, so Review can say so", () => {
    expect(prefillSeed({ prefill: { inlinePolicy: true, state: {} } })?.origin).toEqual({ kind: "clone", policyDropped: true });
    expect(prefillSeed({ prefill: { inlinePolicy: false, state: {} } })?.origin).toEqual({ kind: "clone" });
  });

  it("keeps a held tool-approval posture as the harness default, never a quieter run", () => {
    expect(clone({ runType: "agent", mode: "batch", agent: "claude-code", task: "t", toolApprovals: "hold" })?.intent.tools)
      .toEqual([{ id: "claude-code", kind: "harness", default_effect: "hold" }]);
  });
});
