/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { addHarness, clearStarter, removeHarness, setStartPrompt, setStarter, setTask, swapHarness } from "./harness-lens";

const AUTONOMOUS: RunIntent = {
  shape: "agent_task",
  experience: "background",
  tools: [{ id: "claude-code", kind: "harness" }],
  workload: { kind: "agent_task", agent: "claude-code", task: "fix it" },
};
const INTERACTIVE: RunIntent = {
  shape: "agent_task",
  experience: "interactive",
  tools: [{ id: "claude-code", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code", prompt: "start here" },
};

describe("harness lens (U5)", () => {
  it("an autonomous run holds one harness: a second is refused", () => {
    expect(addHarness(AUTONOMOUS, "codex-cli")).toBe(AUTONOMOUS);
    expect(addHarness({ ...AUTONOMOUS, tools: [], workload: { kind: "agent_task", task: "" } }, "codex-cli").workload?.agent).toBe("codex-cli");
  });

  it("an interactive run, an environment and an application take several", () => {
    expect(addHarness(INTERACTIVE, "codex-cli").tools?.map((t) => t.id)).toEqual(["claude-code", "codex-cli"]);
    const env: RunIntent = { shape: "environment", experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }] };
    expect(addHarness(env, "codex-cli").tools).toHaveLength(2);
    const app: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };
    expect(addHarness(app, "codex-cli").startup).toBeUndefined();
  });

  it("removing an agent task's last harness is refused; an environment may hold none", () => {
    expect(removeHarness(AUTONOMOUS, "claude-code")).toBe(AUTONOMOUS);
    expect(removeHarness(INTERACTIVE, "claude-code")).toBe(INTERACTIVE);
    const env: RunIntent = { shape: "environment", experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }] };
    expect(removeHarness(env, "claude-code").tools).toEqual([]);
  });

  it("removing the started harness drops the start and its prompt", () => {
    const two = addHarness(INTERACTIVE, "codex-cli");
    expect(removeHarness(two, "claude-code").startup).toBeUndefined();
  });

  it("starts in one harness at a time, and the prompt follows the started one", () => {
    const two = addHarness(INTERACTIVE, "codex-cli");
    expect(setStarter(two, "claude-code")).toBe(two);
    const moved = setStarter(two, "codex-cli");
    expect(moved.startup).toEqual({ kind: "harness", tool: "codex-cli" });
    expect(clearStarter(moved).startup).toBeUndefined();
  });

  it("writes the starting prompt only on a harness startup, and empty removes it", () => {
    expect(setStartPrompt(INTERACTIVE, "go").startup).toEqual({ kind: "harness", tool: "claude-code", prompt: "go" });
    expect(setStartPrompt(INTERACTIVE, "").startup).toEqual({ kind: "harness", tool: "claude-code" });
    const none: RunIntent = { shape: "agent_task", experience: "interactive", startup: { kind: "none" } };
    expect(setStartPrompt(none, "go")).toBe(none);
    expect(setStartPrompt({ shape: "agent_task" }, "go").startup).toBeUndefined();
  });

  it("swaps a harness in place and keeps the task or prompt", () => {
    const two = addHarness(INTERACTIVE, "codex-cli");
    expect(swapHarness(two, "codex-cli", "claude-code")).toBe(two);
    expect(swapHarness(two, "claude-code", "none").tools?.map((t) => t.id)).toEqual(["none", "codex-cli"]);
    expect(swapHarness(two, "claude-code", "none").startup).toEqual({ kind: "harness", tool: "none", prompt: "start here" });
    const swapped = swapHarness(AUTONOMOUS, "claude-code", "codex-cli");
    expect(swapped.workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: "fix it" });
    expect(setTask(swapped, "next").workload?.agent).toBe("codex-cli");
  });

  it("a harness added beside the older approval hold starts held, and a swap keeps it held", () => {
    const held: RunIntent = { ...INTERACTIVE, tool_approvals: "hold" };
    expect(addHarness(held, "codex-cli").tools?.[1]).toEqual({ id: "codex-cli", kind: "harness", default_effect: "hold" });
    expect(swapHarness(held, "claude-code", "codex-cli").tools?.[0]).toEqual({ id: "codex-cli", kind: "harness", default_effect: "hold" });
  });
});
