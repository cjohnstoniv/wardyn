/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { mapRunMode } from "./run-mode-map";

const MARK = { mode_contract: "0.9" } as const;

describe("mapRunMode — the experience and carriers of each run shape", () => {
  it("Agent · Interactive starts its harness, with the task as its first message", () => {
    expect(mapRunMode({ agent: "claude-code", task: "", interactive: true, interactive_start: "agent" })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code" },
    });
    expect(mapRunMode({ agent: "claude-code", task: "do x", interactive: true, interactive_start: "agent" })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code", prompt: "do x" },
    });
  });

  it("Environment with a terminal starts nothing, or its command", () => {
    expect(mapRunMode({ agent: "codex-cli", task: "", interactive: true })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }],
    });
    expect(mapRunMode({ agent: "codex-cli", task: "npm ci", interactive: true, interactive_start: "shell" })).toEqual({
      ...MARK, experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }], startup: { kind: "command", command: "npm ci" },
    });
    // No agent: an environment with no harness, on the agent base image.
    expect(mapRunMode({ interactive: true })).toEqual({ ...MARK, experience: "interactive" });
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

  // A template leaves a background run's text, and may leave its agent, for the person who uses it.
  it("maps a template's background run whose text or agent is left for later", () => {
    expect(mapRunMode({ agent: "claude-code", interactive: false, task_mode: "harness" }, true)).toEqual({
      ...MARK, experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "" }, tools: [{ id: "claude-code", kind: "harness" }],
    });
    expect(mapRunMode({ interactive: false, task_mode: "harness" }, true)).toEqual({ ...MARK, experience: "background", workload: { kind: "agent_task", task: "" } });
    expect(mapRunMode({ interactive: false, task: "fix it" }, true)).toEqual({ ...MARK, experience: "background", workload: { kind: "agent_task", task: "fix it" } });
    expect(mapRunMode({ interactive: false, task_mode: "exec" }, true)).toEqual({ ...MARK, experience: "background", workload: { kind: "command", command: "" } });
    expect(mapRunMode({ interactive: false, task_mode: "harness" })).toBeNull();
  });

  it.each([
    ["an interactive task with no interactive_start", { agent: "claude-code", task: "do x", interactive: true }],
    ["a harness start with no agent", { task: "", interactive: true, interactive_start: "agent" as const }],
    ["a model provider with no agent", { task: "make test", interactive: false, task_mode: "exec" as const, model_provider: "p1" }],
    ["the boot-seed opt-in", { agent: "claude-code", task: "do x", interactive: true, interactive_start: "agent" as const, seed_auto_tools: true }],
    ["held tool approvals", { agent: "claude-code", task: "do x", tool_approvals: "hold" as const }],
    ["a command that names an agent", { agent: "claude-code", task: "ls", task_mode: "exec" as const }],
    ["an agent task with no prompt", { agent: "claude-code", task: "  " }],
  ])("%s has no carrier and keeps the older fields", (_name, run) => {
    expect(mapRunMode(run)).toBeNull();
  });
});
