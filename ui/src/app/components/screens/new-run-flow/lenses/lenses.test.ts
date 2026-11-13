/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { LEGACY_RUN_MODE_FIELDS } from "../../../../lib/run-mode-map";
import { intentToWire } from "../draft/intent-to-wire";
import type { RunIntent } from "../draft/run-intent";
import { attachWorkspace, chooseNothing, detachWorkspace, setStartsHere } from "./files-lens";
import { addHarness, giveBackText, removeHarness, setStarter, setTask, startWithHeld, textLeft } from "./harness-lens";
import { setCpu, setMemory } from "./limits-lens";
import { firstInvalidStep, localIssues, stepIssues } from "./local-issues";
import { setCommand } from "./apps-lens";
import { chooseBarrier, chooseMachineKind, chooseMode, choosePool } from "./run-lens";
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
  tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness", task: "write tests" }],
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
  ["chooseMachineKind linux_vm", (i) => chooseMachineKind(i, "linux_vm")],
  ["chooseMachineKind container", (i) => chooseMachineKind(i, "container")],
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
    expect(changed(START, chooseShape(START, "application"))).toEqual(["shape", "tools", "workload"]);
  });
});

describe("lens edits", () => {
  it("say DS-5's four draft rows", () => {
    const one: RunIntent = { tools: [{ id: "claude-code", kind: "harness" }], workload: { kind: "agent_task", task: "t" } };
    expect(shapeCarriers("agent_task", "interactive", one)).toEqual({ experience: "interactive", workload: undefined, startup: { kind: "harness", tool: "claude-code", prompt: "t" } });
    expect(shapeCarriers("agent_task", "autonomous", one)).toEqual({ experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "t" }, startup: undefined });
    expect(shapeCarriers("environment", undefined, one)).toEqual({ experience: "interactive", workload: undefined, startup: undefined });
    expect(shapeCarriers("application", undefined, one)).toEqual({ experience: "background", workload: { kind: "command", command: "" }, startup: undefined });
  });

  it("choosing the shape the draft already has changes nothing: the starter, its prompt and the task stay", () => {
    const tools: RunIntent["tools"] = [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }];
    const interactive: RunIntent = { shape: "agent_task", experience: "interactive", tools, startup: { kind: "harness", tool: "codex-cli", prompt: "Review the diff" } };
    expect(chooseShape(interactive, "agent_task")).toBe(interactive);
    const autonomous: RunIntent = { shape: "agent_task", experience: "background", tools, workload: { kind: "agent_task", agent: "codex-cli", task: "fix it" } };
    expect(chooseShape(autonomous, "agent_task")).toBe(autonomous);
    const environment: RunIntent = { shape: "environment", experience: "interactive", tools, startup: { kind: "harness", tool: "claude-code" } };
    expect(chooseShape(environment, "environment")).toBe(environment);
    // Another shape still writes its own carriers.
    expect(chooseShape(interactive, "environment")).toMatchObject({ shape: "environment", experience: "interactive" });
    expect(chooseShape(interactive, "environment").startup).toBeUndefined();
  });

  describe("the person's text crosses a mode switch, only to the agent it belongs to", () => {
    const tools: RunIntent["tools"] = [{ id: "claude-code", kind: "harness" }];
    const autonomous: RunIntent = { shape: "agent_task", experience: "background", tools, workload: { kind: "agent_task", agent: "claude-code", task: "fix it" } };
    const interactive: RunIntent = { shape: "agent_task", experience: "interactive", tools, startup: { kind: "harness", tool: "claude-code", prompt: "fix it" } };

    it("Autonomous to Interactive: the task is the started harness's first message", () => {
      expect(chooseMode(autonomous, "interactive")).toEqual(interactive);
      // A blank task is none: the server refuses a blank starting prompt.
      const startOf = (task: string) => chooseMode({ ...autonomous, workload: { kind: "agent_task", agent: "claude-code", task } }, "interactive").startup;
      expect(startOf("")).toEqual({ kind: "harness", tool: "claude-code" });
      expect(startOf("  ")).toEqual({ kind: "harness", tool: "claude-code" });
      // Your own tools starts no CLI, so the server refuses it a prompt.
      const none: RunIntent = { ...autonomous, tools: [{ id: "none", kind: "harness" }], workload: { kind: "agent_task", agent: "none", task: "fix it" } };
      expect(chooseMode(none, "interactive").startup).toEqual({ kind: "harness", tool: "none" });
      // Interactive starts in the agent the task belongs to, whichever harness it is.
      const second: RunIntent = { ...autonomous, tools: [...tools, { id: "codex-cli", kind: "harness" }], workload: { kind: "agent_task", agent: "codex-cli", task: "fix it" } };
      expect(chooseMode(second, "interactive").startup).toEqual({ kind: "harness", tool: "codex-cli", prompt: "fix it" });
    });

    it("Interactive keeps a harness the draft already starts in, with its first message", () => {
      const second: RunIntent = { shape: "agent_task", tools: [...tools, { id: "codex-cli", kind: "harness" }], startup: { kind: "harness", tool: "codex-cli", prompt: "review" } };
      expect(chooseMode(second, "interactive")).toEqual({ ...second, experience: "interactive" });
      // A start that names a harness the draft does not include is not kept: the server refuses it.
      expect(chooseMode({ ...second, tools }, "interactive").startup).toEqual({ kind: "harness", tool: "claude-code" });
    });

    it.each(["linux_vm", "macos_vm"] as const)("Interactive on a %s starts no harness, and the task is left out of the draft", (kind) => {
      const vm: RunIntent = { ...autonomous, machine_kind: kind };
      const next = chooseMode(vm, "interactive");
      expect(next).toEqual({ shape: "agent_task", experience: "interactive", tools, machine_kind: kind });
      expect(textLeft(vm, next)).toEqual({ "claude-code": "fix it" });
      // A template's agent in the older field, its run mode left to the person: the same, with no older field left.
      expect(chooseMode({ shape: "agent_task", agent: "claude-code", machine_kind: kind }, "interactive")).toEqual({ shape: "agent_task", experience: "interactive", tools, machine_kind: kind });
    });

    it("Interactive to Autonomous: the started harness's first message is the task", () => {
      expect(chooseMode(interactive, "autonomous")).toEqual(autonomous);
      const taskOf = (startup: RunIntent["startup"], t: RunIntent["tools"] = tools) => chooseMode({ ...interactive, tools: t, startup }, "autonomous").workload;
      const empty = { kind: "agent_task", agent: "claude-code", task: "" };
      expect(taskOf({ kind: "harness", tool: "claude-code", prompt: "  " })).toEqual(empty);
      // Autonomous runs the started harness, so its first message is its task; a startup command carries nothing.
      expect(taskOf({ kind: "harness", tool: "codex-cli", prompt: "review" }, [...tools, { id: "codex-cli", kind: "harness" }])).toEqual({ kind: "agent_task", agent: "codex-cli", task: "review" });
      expect(taskOf({ kind: "command", command: "npm ci" })).toEqual(empty);
      // A draft that names one agent for its task and starts another: the other's first message is not that agent's task.
      const both: RunIntent = { ...interactive, tools: [...tools, { id: "codex-cli", kind: "harness" }], workload: { kind: "agent_task", agent: "claude-code", task: "" }, startup: { kind: "harness", tool: "codex-cli", prompt: "review" } };
      expect(chooseMode(both, "autonomous").workload).toEqual(empty);
    });

    // What Interactive has no place for is held beside the draft, and a switch back gives it to its agent where that is empty.
    it("text Interactive has no place for is held, never in the draft, and Autonomous gives it back", () => {
      const roundTrip = (from: RunIntent) => {
        const there = chooseMode(from, "interactive");
        return { there, held: textLeft(from, there), back: giveBackText(chooseMode(there, "autonomous"), textLeft(from, there)) };
      };
      // The Runs composer's task, before any harness is in the draft: Interactive writes no startup.
      const composer: RunIntent = { shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "fix it" } };
      expect(roundTrip(composer)).toEqual({ there: { shape: "agent_task", experience: "interactive" }, held: { "": "fix it" }, back: composer });
      // Another agent's own task: Interactive starts one harness and has no place for the rest.
      const two: RunIntent = { ...autonomous, tools: [...tools, { id: "codex-cli", kind: "harness", task: "review" }] };
      expect(roundTrip(two)).toMatchObject({ held: { "codex-cli": "review" }, back: two });
      // Your own tools takes no first message.
      const none: RunIntent = { ...autonomous, tools: [{ id: "none", kind: "harness" }], workload: { kind: "agent_task", agent: "none", task: "fix it" } };
      expect(roundTrip(none)).toMatchObject({ held: { none: "fix it" }, back: none });
      // Text Interactive carries is not held, and a blank text is none.
      expect(roundTrip(autonomous).held).toBeUndefined();
      expect(roundTrip({ ...composer, workload: { kind: "agent_task", task: "  " } }).held).toBeUndefined();
      expect(textLeft({ ...interactive, startup: { kind: "harness", tool: "claude-code", prompt: "  " } }, composer)).toBeUndefined();
      expect(textLeft({ shape: "agent_task", tools: [{ id: "codex-cli", kind: "harness", task: "  " }] }, composer)).toBeUndefined();
      // Held text never writes over a task the person typed since.
      expect(giveBackText(composer, { "": "older" })).toEqual(composer);
      expect(giveBackText(two, { "codex-cli": "older" })).toEqual(two);
    });

    it("a harness the person starts takes the text held for it, or one written before any harness; Your own tools takes none", () => {
      const two: RunIntent = { shape: "agent_task", experience: "interactive", tools: [...tools, { id: "codex-cli", kind: "harness" }] };
      expect(startWithHeld(two, "codex-cli", { "codex-cli": "review", "": "fix it" }).startup).toEqual({ kind: "harness", tool: "codex-cli", prompt: "review" });
      expect(startWithHeld(two, "claude-code", { "codex-cli": "review", "": "fix it" }).startup).toEqual({ kind: "harness", tool: "claude-code", prompt: "fix it" });
      expect(startWithHeld(two, "claude-code", { "codex-cli": "review" }).startup).toEqual({ kind: "harness", tool: "claude-code" });
      expect(startWithHeld({ ...two, tools: [{ id: "none", kind: "harness" }] }, "none", { "": "fix it" }).startup).toEqual({ kind: "harness", tool: "none" });
      expect(startWithHeld(interactive, "claude-code", { "": "other" })).toBe(interactive);
    });

    it("another shape takes no agent text: a task or a prompt never becomes a command", () => {
      expect(chooseShape(autonomous, "application").workload).toEqual({ kind: "command", command: "" });
      expect(chooseShape(interactive, "application").workload).toEqual({ kind: "command", command: "" });
    });
  });

  it("never removes an agent task's last harness, and the next one's own task becomes the run's", () => {
    const one = removeHarness(START, "codex-cli");
    expect(removeHarness(one, "claude-code")).toBe(one);
    expect(removeHarness(START, "claude-code").workload).toEqual({ kind: "agent_task", agent: "codex-cli", task: "write tests" });
  });

  it("writes an added host as a run override, never as policy", () => {
    const next = addAllowedHost(START, "api.example");
    expect(next.overrides).toEqual({ hosts: { add_allowed: ["api.example"] } });
    expect(next.inline_policy).toBe(START.inline_policy);
  });
});

// A template that leaves its run mode to the person may still name its agent or task in the older fields, which the
// server refuses beside the carriers Guided setup writes: the person's choice of shape or mode converts them.
describe("a draft in the older run-mode fields converts when the person names the mode", () => {
  const agentTask = (i: RunIntent) => chooseShape(i, "agent_task");
  it.each<[string, RunIntent, (i: RunIntent) => RunIntent, RunIntent]>([
    ["an agent preset run autonomously", { agent: "codex-cli" }, (i) => chooseMode(agentTask(i), "autonomous"),
      { shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "codex-cli", task: "" }, tools: [{ id: "codex-cli", kind: "harness" }] }],
    ["an agent preset run interactively", { agent: "codex-cli" }, (i) => chooseMode(agentTask(i), "interactive"),
      { shape: "agent_task", experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }], startup: { kind: "harness", tool: "codex-cli" } }],
    ["an agent and task run interactively", { agent: "claude-code", task: "review the diff" }, (i) => chooseMode(agentTask(i), "interactive"),
      { shape: "agent_task", experience: "interactive", tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code", prompt: "review the diff" } }],
    ["a task chosen as an application", { task: "make test" }, (i) => chooseShape(i, "application"),
      { shape: "application", experience: "background", workload: { kind: "command", command: "make test" } }],
    ["an agent preset chosen as an environment", { agent: "codex-cli", image: "registry.example/app:1" }, (i) => chooseShape(i, "environment"),
      { shape: "environment", experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }], image: "registry.example/app:1" }],
    ["an agent and task run autonomously after an environment that could not say them", { agent: "codex-cli", task: "fix the flaky test" },
      (i) => chooseMode(agentTask(chooseShape(i, "environment")), "autonomous"),
      { shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "codex-cli", task: "fix the flaky test" }, tools: [{ id: "codex-cli", kind: "harness" }] }],
    // The draft's own older field stands over what the choice says: a task it calls a harness prompt is never made a command.
    ["a harness task chosen as an application", { task: "fix the bug", task_mode: "harness" }, (i) => chooseShape(i, "application"),
      { shape: "application", experience: "background", workload: { kind: "command", command: "" } }],
    ["an older field with no agent or task run interactively", { task_mode: "harness" }, (i) => chooseMode(agentTask(i), "interactive"), { shape: "agent_task", experience: "interactive" }],
    // A template may carry its shape: choosing that shape again still converts.
    ["an agent preset whose template carries the environment shape", { shape: "environment", agent: "codex-cli" }, (i) => chooseShape(i, "environment"),
      { shape: "environment", experience: "interactive", tools: [{ id: "codex-cli", kind: "harness" }] }],
    // Carriers converted to the chosen shape are kept as converted: the shell command is not cleared.
    ["a shell command chosen as an environment", { task: "npm ci", interactive_start: "shell" }, (i) => chooseShape(i, "environment"),
      { shape: "environment", experience: "interactive", startup: { kind: "command", command: "npm ci" } }],
    // The run-mode fields an earlier choice wrote beside the older ones are set aside, then written again.
    ["a task chosen as an application after an environment that could not say it", { task: "make test" }, (i) => chooseShape(chooseShape(i, "environment"), "application"),
      { shape: "application", experience: "background", workload: { kind: "command", command: "make test" } }],
  ])("%s", (_name, seeded, ask, carried) => {
    const intent = ask(seeded);
    expect(intent).toEqual(carried);
    const { body, unsupported } = intentToWire(intent);
    expect(unsupported).toEqual([]);
    for (const field of LEGACY_RUN_MODE_FIELDS) expect(body).not.toHaveProperty(field);
  });

  it.each<[string, RunIntent, (i: RunIntent) => RunIntent]>([
    // A task in an environment may be a first prompt or a startup command: only the person can say which.
    ["an environment with a task", { agent: "codex-cli", task: "npm ci" }, (i) => chooseShape(i, "environment")],
    // A draft that states its run mode was converted when it was seeded, or holds what the carriers cannot say.
    ["a draft that states interactive", { agent: "claude-code", task: "npm ci", interactive: true }, (i) => chooseMode(agentTask(i), "autonomous")],
    ["a draft that states interactive, run interactively", { agent: "claude-code", task: "npm ci", interactive: true }, (i) => chooseMode(agentTask(i), "interactive")],
    // An empty or false older field says nothing (the SDK writes an empty `agent`): the run-mode fields beside it are the run.
    ["run-mode fields beside an empty older field", { agent: "", shape: "application", experience: "background", workload: { kind: "command", command: "make test" } }, (i) => chooseShape(i, "application")],
    ["run-mode fields beside a false older field", { seed_auto_tools: false, shape: "agent_task", experience: "background", workload: { kind: "agent_task", task: "fix it" } }, (i) => chooseMode(i, "autonomous")],
    // Tools the person chose are not weighed against the older agent: the draft is repaired in the document.
    ["a draft that holds tools", { agent: "codex-cli", tools: [{ id: "claude-code", kind: "harness" }] }, (i) => chooseMode(agentTask(i), "autonomous")],
  ])("keeps the older fields for %s", (_name, seeded, ask) => {
    expect(ask(seeded)).toMatchObject(seeded);
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

describe("chooseMachineKind", () => {
  it("a VM drops the container barrier; a container is the absent key", () => {
    const vm = chooseMachineKind({ confinement_class: "CC2", runner_pool_id: "p" }, "linux_vm");
    expect(vm).toEqual({ machine_kind: "linux_vm", runner_pool_id: "p" });
    expect(chooseMachineKind(vm, "container")).toEqual({ runner_pool_id: "p" });
  });

  it("a VM clears the harness that starts the run, and keeps a command startup (UXR-1)", () => {
    const started = { shape: "agent_task", experience: "interactive", startup: { kind: "harness", tool: "claude-code", prompt: "go" } } as const;
    for (const kind of ["linux_vm", "macos_vm"] as const) expect(chooseMachineKind(started, kind).startup).toBeUndefined();
    expect(chooseMachineKind(started, "container").startup).toEqual(started.startup);
    const server = { shape: "application", experience: "interactive", startup: { kind: "command", command: "npm start" } } as const;
    expect(chooseMachineKind(server, "linux_vm").startup).toEqual(server.startup);
  });
});
