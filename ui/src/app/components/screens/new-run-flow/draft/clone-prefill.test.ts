/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AuditEvent } from "../../../../lib/types";
import { cloneFromAudit, runPrefill } from "./clone-prefill";
import { intentToWire } from "./intent-to-wire";
import { prefillSeed } from "./prefill-seed";

// A killed run as the two sources leave it behind.
const row = {
  agent: "codex-cli",
  task: "rerun the migration",
  policy_id: "pol-7",
  confinement_class: "CC3" as const,
  interactive: true,
  workspace_ids: ["ws-a", "ws-b"],
};
// ...and what createRunAuditData stamps that the row cannot hold.
const created = { task_mode: "exec", interactive_start: "shell", tool_approvals: "hold", inline_policy: false };

describe("runPrefill: the clone carries both sources, and says what it cannot", () => {
  it("round-trips every field the two sources carry", () => {
    expect(runPrefill(row, created).state).toEqual({
      runType: "command",
      agent: "codex-cli",
      command: "rerun the migration",
      mode: "interactive",
      interactiveStart: "shell",
      toolApprovals: "hold",
      confinementClass: "CC3",
      selectedPolicyId: "pol-7",
      workspaces: [{ workspaceId: "ws-a" }, { workspaceId: "ws-b" }],
    });
  });

  it("types the one text into the field the audit row says it was typed in", () => {
    expect(runPrefill(row, { interactive_start: "shell" }).state).toMatchObject({ startupCommand: "rerun the migration" });
    expect(runPrefill({ ...row, interactive: false }).state).toMatchObject({ task: "rerun the migration", mode: "batch" });
  });

  it("an inline-policy run clones without its policy and says so, keeping the barrier", () => {
    const prefill = runPrefill({ ...row, policy_id: undefined }, { ...created, inline_policy: true });
    expect(prefill.inlinePolicy).toBe(true);
    expect(prefill.state.selectedPolicyId).toBeUndefined();
    expect(prefill.state.confinementClass).toBe("CC3");
    expect(prefillSeed({ prefill })?.origin).toEqual({ kind: "clone", policyDropped: true });
  });

  it("an older trail with no run.create data reads as not asked for, never as a guess", () => {
    expect(runPrefill(row).state).toMatchObject({ runType: "agent", interactiveStart: "agent", toolApprovals: "auto" });
  });

  it("carries no title, description, image or credential field", () => {
    const keys = Object.keys(runPrefill(row, created).state);
    for (const k of ["title", "description", "image", "seedAutoTools", "llmSecretName"]) expect(keys).not.toContain(k);
  });

  it("survives structured clone, as router history state does", () => {
    const prefill = runPrefill(row, created);
    expect(structuredClone(prefill)).toEqual(prefill);
  });
});

describe("cloneFromAudit", () => {
  const event = (action: string, extra: object = {}) => ({ action, ...extra }) as unknown as AuditEvent;

  it("is null without a run.create row, so no door launches a clone with defaults", () => {
    expect(cloneFromAudit(row, [])).toBeNull();
    expect(cloneFromAudit(row, [event("run.kill")])).toBeNull();
  });

  it("builds the prefill when the run.create row exists", () => {
    expect(cloneFromAudit(row, [event("run.create")])?.state).toMatchObject({ agent: "codex-cli", confinementClass: "CC3" });
  });
});

// The fallback a run with no stored launch intent clones through, end to end: audit row to request body.
describe("cloneFromAudit → prefillSeed → intentToWire", () => {
  const agentRun = { agent: "claude-code", task: "fix the flaky test", confinement_class: "CC2" as const, interactive: true };
  const wire = (run: typeof agentRun, data: Record<string, unknown> = {}) => {
    const prefill = cloneFromAudit(run, [{ action: "run.create", data } as unknown as AuditEvent]);
    return intentToWire(prefillSeed({ prefill })!.intent).body as Record<string, unknown>;
  };

  it("an interactive agent run's task arrives as the first prompt, with no automatic tools, title or description", () => {
    const body = wire(agentRun);
    expect(body.startup).toEqual({ kind: "harness", tool: "claude-code", prompt: "fix the flaky test" });
    expect(body).not.toHaveProperty("seed_auto_tools");
    expect(body).not.toHaveProperty("title");
    expect(body).not.toHaveProperty("description");
  });

  it("a blank task sends no prompt", () => {
    expect(wire({ ...agentRun, task: "  " }).startup).toEqual({ kind: "harness", tool: "claude-code" });
    expect(wire({ ...agentRun, task: "" }).startup).toEqual({ kind: "harness", tool: "claude-code" });
  });

  it("a held run keeps its hold beside the prompt", () => {
    const body = wire(agentRun, { tool_approvals: "hold" });
    expect(body.tools).toEqual([{ id: "claude-code", kind: "harness", default_effect: "hold" }]);
    expect(body.startup).toEqual({ kind: "harness", tool: "claude-code", prompt: "fix the flaky test" });
  });

  it("shell and background runs are as before: the text is their command or task, never a prompt", () => {
    expect(wire(agentRun, { interactive_start: "shell" }).startup).toEqual({ kind: "command", command: "fix the flaky test" });
    const background = wire({ ...agentRun, interactive: false });
    expect(background.workload).toEqual({ kind: "agent_task", agent: "claude-code", task: "fix the flaky test" });
    expect(background).not.toHaveProperty("startup");
    expect(wire({ ...agentRun, interactive: false }, { task_mode: "exec" }).workload).toEqual({ kind: "command", command: "fix the flaky test" });
  });
});
