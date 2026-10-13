/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { mapRunMode } from "./run-mode-map";

const MARK = { mode_contract: "0.9" } as const;

describe("mapRunMode — the experience and carriers of each run shape", () => {
  it("Agent · Interactive starts its harness", () => {
    expect(mapRunMode({ agent: "claude-code", task: "", interactive: true, interactive_start: "agent" })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code" },
    });
  });

  it("Environment with a terminal starts nothing, or its command", () => {
    expect(mapRunMode({ agent: "codex-cli", task: "", interactive: true })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }],
    });
    expect(mapRunMode({ agent: "codex-cli", task: "npm ci", interactive: true, interactive_start: "shell" })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }], startup: { kind: "command", command: "npm ci" },
    });
  });

  it("Agent · Autonomous runs the agent task with only that harness", () => {
    expect(mapRunMode({ agent: "claude-code", task: "fix it", interactive: false, model_provider: "p1" })).toEqual({
      ...MARK,
      experience: "background",
      workload: { kind: "agent_task", agent: "claude-code", task: "fix it" },
      tools: [{ id: "claude-code", kind: "harness", model_provider: "p1" }],
    });
  });

  it("Application · Automation runs the command and includes no tool", () => {
    expect(mapRunMode({ task: "make test", interactive: false, task_mode: "exec" })).toEqual({
      ...MARK, experience: "background", workload: { kind: "command", command: "make test" },
    });
  });

  it.each([
    ["an interactive initial prompt", { agent: "claude-code", task: "do x", interactive: true, interactive_start: "agent" as const }],
    ["the boot-seed opt-in", { agent: "claude-code", task: "do x", interactive: true, interactive_start: "agent" as const, seed_auto_tools: true }],
    ["held tool approvals", { agent: "claude-code", task: "do x", tool_approvals: "hold" as const }],
    ["a command that names an agent", { agent: "claude-code", task: "ls", task_mode: "exec" as const }],
    ["an agent task with no prompt", { agent: "claude-code", task: "  " }],
  ])("%s has no carrier and keeps the older fields", (_name, run) => {
    expect(mapRunMode(run)).toBeNull();
  });
});
