/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { addHarness, agentTask, clearStarter, harnessIssues, removeHarness, setAgentTask, setStartPrompt, setStarter, setTask, swapHarness } from "./harness-lens";
import { chooseMode } from "./run-lens";

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

describe("harness lens: several agents (F2MULTI)", () => {
  const two = (): RunIntent => setAgentTask(addHarness(AUTONOMOUS, "codex-cli", true), "codex-cli", "write tests");

  it("only with the switch on does an autonomous run take a second harness", () => {
    expect(addHarness(AUTONOMOUS, "codex-cli")).toBe(AUTONOMOUS);
    expect(addHarness(AUTONOMOUS, "codex-cli", true).tools?.map((t) => t.id)).toEqual(["claude-code", "codex-cli"]);
  });

  it("the run's agent keeps workload.task; every other harness carries its own, and empty removes it", () => {
    const next = two();
    expect(next.workload).toEqual({ kind: "agent_task", agent: "claude-code", task: "fix it" });
    expect(next.tools?.[1]).toEqual({ id: "codex-cli", kind: "harness", task: "write tests" });
    expect(agentTask(next, "claude-code")).toBe("fix it");
    expect(agentTask(next, "codex-cli")).toBe("write tests");
    expect(setAgentTask(next, "codex-cli", "").tools?.[1]).toEqual({ id: "codex-cli", kind: "harness" });
  });

  it("a swap keeps the card's task, and removing the run's agent hands its task slot to the next agent", () => {
    expect(swapHarness(two(), "codex-cli", "gemini-cli").tools?.[1]).toEqual({ id: "gemini-cli", kind: "harness", task: "write tests" });
    const left = removeHarness(two(), "claude-code");
    expect(left.tools).toEqual([{ id: "codex-cli", kind: "harness" }]);
    expect(left.workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: "write tests" });
  });

  it("each agent needs a task, and the first without one is where Continue lands", () => {
    expect(harnessIssues(two(), {})).toEqual([]);
    const empty = setAgentTask(two(), "codex-cli", "");
    expect(harnessIssues(empty, {}).map((i) => [i.code, i.focus])).toEqual([["agent_task_required", "nr-task-1"]]);
    expect(harnessIssues(setTask(empty, ""), {}).map((i) => i.focus)).toEqual(["nr-task", "nr-task-1"]);
    expect(harnessIssues(AUTONOMOUS, {})).toEqual([]);
  });

  it("leaving Autonomous takes the harnesses' own tasks off the tools", () => {
    const interactive = chooseMode(two(), "interactive");
    expect(interactive.tools?.some((t) => t.task !== undefined)).toBe(false);
    expect(interactive.tools).toHaveLength(2);
  });
});
