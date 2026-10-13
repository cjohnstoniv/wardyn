/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { attachWorkspace, chooseNothing, detachWorkspace, setStartsHere } from "./files-lens";
import { addHarness, removeHarness, setStarter, setTask } from "./harness-lens";
import { setCpu, setMemory } from "./limits-lens";
import { firstInvalidStep, localIssues, stepIssues } from "./local-issues";
import { chooseBarrier, chooseMode, choosePool, setCommand } from "./run-lens";
import { chooseShape, shapeCarriers } from "./shape-lens";
import { addAllowedHost, setModelProvider } from "./tools-lens";

// Keys no Guided step owns: a lens must hand each back exactly as it found it.
const DOCUMENT_ONLY = {
  image: "registry.example/app:1",
  devcontainer_ref: "main",
  ends_after_sec: 3600,
  placement: "remote",
  inline_policy: { auto_stop_after_sec: 900, allowed_domains: ["a.example"] },
} as const satisfies RunIntent;

const START: RunIntent = {
  ...DOCUMENT_ONLY,
  shape: "agent_task",
  experience: "background",
  workload: { kind: "agent_task", agent: "claude-code", task: "fix it" },
  tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }],
  workspaces: [{ workspace_id: "w1" }],
  start_folder: { kind: "attachment", attachment: "w1" },
};

const EDITS: [string, (i: RunIntent) => RunIntent][] = [
  ["chooseShape agent_task", (i) => chooseShape(i, "agent_task")],
  ["chooseShape environment", (i) => chooseShape(i, "environment")],
  ["chooseShape application", (i) => chooseShape(i, "application")],
  ["chooseMode interactive", (i) => chooseMode(i, "interactive")],
  ["chooseMode autonomous", (i) => chooseMode(i, "autonomous")],
  ["setCommand", (i) => setCommand(i, "make test")],
  ["choosePool", (i) => choosePool(i, "pool-2")],
  ["chooseBarrier", (i) => chooseBarrier(i, "CC3")],
  ["addHarness", (i) => addHarness(i, "gemini")],
  ["removeHarness", (i) => removeHarness(i, "codex-cli")],
  ["setStarter", (i) => setStarter(i, "claude-code")],
  ["setTask", (i) => setTask(i, "write docs")],
  ["attachWorkspace", (i) => attachWorkspace(i, "w2")],
  ["detachWorkspace", (i) => detachWorkspace(i, "w1")],
  ["setStartsHere", (i) => setStartsHere(i, "w1")],
  ["chooseNothing", (i) => chooseNothing(i)],
  ["addAllowedHost", (i) => addAllowedHost(i, "api.example")],
  ["setModelProvider", (i) => setModelProvider(i, "claude-code", "anthropic")],
  ["setCpu", (i) => setCpu(i, 2000)],
  ["setMemory", (i) => setMemory(i, 4096)],
];

describe("lenses keep what they do not own (U4)", () => {
  it.each(EDITS)("%s never removes or changes a document-only key", (_name, edit) => {
    const before = structuredClone(START);
    const after = edit(START);
    for (const [key, value] of Object.entries(DOCUMENT_ONLY)) expect(after[key as keyof RunIntent], key).toEqual(value);
    expect(START).toEqual(before);
  });

  it("each edit changes only its own keys", () => {
    const changed = (a: RunIntent, b: RunIntent) =>
      [...new Set([...Object.keys(a), ...Object.keys(b)])].filter((k) => JSON.stringify(a[k as keyof RunIntent]) !== JSON.stringify(b[k as keyof RunIntent])).sort();
    expect(changed(START, choosePool(START, "p"))).toEqual(["runner_pool_id"]);
    expect(changed(START, setCpu(START, 1000))).toEqual(["resources"]);
    expect(changed(START, addAllowedHost(START, "h.example"))).toEqual(["overrides"]);
    expect(changed(START, setStartsHere(START, "w1"))).toEqual([]);
    expect(changed(START, chooseShape(START, "application"))).toEqual(["shape", "workload"]);
  });
});

describe("lens edits", () => {
  it("say DS-5's four draft rows", () => {
    const one: RunIntent = { tools: [{ id: "claude-code", kind: "harness" }], workload: { kind: "agent_task", task: "t" } };
    expect(shapeCarriers("agent_task", "interactive", one)).toEqual({ experience: "interactive", workload: undefined, startup: { kind: "harness", tool: "claude-code" } });
    expect(shapeCarriers("agent_task", "autonomous", one)).toEqual({ experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "t" }, startup: undefined });
    expect(shapeCarriers("environment", undefined, one)).toEqual({ experience: "interactive", workload: undefined, startup: undefined });
    expect(shapeCarriers("application", undefined, one)).toEqual({ experience: "background", workload: { kind: "command", command: "" }, startup: undefined });
  });

  it("never removes an agent task's last harness, and moves the task to the next one", () => {
    const one = removeHarness(START, "codex-cli");
    expect(removeHarness(one, "claude-code")).toBe(one);
    expect(removeHarness(START, "claude-code").workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: "fix it" });
  });

  it("writes an added host as a run override, never as policy", () => {
    const next = addAllowedHost(START, "api.example");
    expect(next.overrides).toEqual({ hosts: { add_allowed: ["api.example"] } });
    expect(next.inline_policy).toBe(START.inline_policy);
  });
});

describe("local validators", () => {
  it("name the first incomplete step and every issue in step order", () => {
    expect(firstInvalidStep({})).toBe("shape");
    expect(localIssues({}).map((i) => i.code)).toEqual(["shape_required", "files_choice_required"]);
    const task: RunIntent = { shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: " " } };
    expect(stepIssues("harnesses", task).map((i) => i.code)).toEqual(["harness_required", "task_required"]);
    expect(firstInvalidStep(task)).toBe("harnesses");
    expect(stepIssues("run", { shape: "application", workload: { kind: "command", command: "" } })[0]).toEqual({ home: "run", code: "command_required", focus: "nr-command" });
    expect(stepIssues("run", {}, { poolDefault: false }).map((i) => i.code)).toEqual(["pool_required"]);
    expect(stepIssues("run", {})).toEqual([]);
    expect(stepIssues("files", { workspaces: [{ workspace_id: "w1" }] }).map((i) => i.code)).toEqual(["starts_here_required"]);
    expect(stepIssues("files", { no_repositories_or_drives: true })).toEqual([]);
    expect(firstInvalidStep(START)).toBeUndefined();
    expect(localIssues(START, {}, { format: "yaml", text: "", diagnostics: [], validated: false }).map((i) => i.code)).toEqual(["doc_invalid"]);
  });
});
