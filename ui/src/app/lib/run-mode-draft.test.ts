/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import {
  buildRunContractWire,
  buildRunModeWire,
  effectiveExperience,
  emptyRunContractDraft,
  emptyRunModeDraft,
  inactiveRunMode,
  NO_ACTIVE_SECTIONS,
  pushRuleKey,
  runModeBlocker,
  withoutTool,
  type RunModeDraft,
} from "./run-contract-draft";
import { RUN_MODE_REASON, RUN_MODE_REFUSAL } from "./run-mode-refusals";

const claude = { id: "claude-code", kind: "harness", model_provider: "bedrock-team" } as const;
const codex = { id: "codex-cli", kind: "harness" } as const;

describe("run mode draft", () => {
  it("a fresh draft has no mode and sends none of the run-mode fields", () => {
    expect(emptyRunContractDraft().mode).toEqual({ tools: [] });
    expect(buildRunModeWire(emptyRunModeDraft())).toEqual({});
    // Nothing is inferred: a draft holding everything but an experience still sends nothing.
    const held: RunModeDraft = {
      tools: [claude],
      workload: { kind: "command", command: "make" },
      startup: { kind: "none" },
      startFolder: { kind: "image_default" },
      noRepositoriesOrDrives: true,
    };
    expect(buildRunModeWire(held)).toEqual({});
    expect(buildRunModeWire(undefined)).toEqual({});
  });

  it("Background sends the workload and only the harness its agent task runs, with no startup", () => {
    const mode: RunModeDraft = {
      experience: "background",
      workload: { kind: "agent_task", agent: "claude-code", task: "fix" },
      tools: [claude, codex],
      startup: { kind: "harness", tool: "codex-cli" },
      noRepositoriesOrDrives: true,
    };
    expect(buildRunModeWire(mode)).toEqual({
      experience: "background",
      workload: mode.workload,
      tools: [claude],
      no_repositories_or_drives: true,
    });
    // A command runs no harness at all.
    expect(buildRunModeWire({ ...mode, workload: { kind: "command", command: "make" } }).tools).toBeUndefined();
    // What the draft keeps for the other mode is reported, not sent.
    expect(inactiveRunMode(mode)).toEqual({ startup: mode.startup });
  });

  it("Interactive sends the tools and the one startup, with no workload", () => {
    const mode: RunModeDraft = {
      experience: "interactive",
      workload: { kind: "command", command: "make" },
      tools: [claude, codex],
      startup: { kind: "harness", tool: "claude-code" },
      startFolder: { kind: "attachment", attachment: "drive", subpath: "src" },
    };
    expect(buildRunModeWire(mode)).toEqual({
      experience: "interactive",
      tools: [claude, codex],
      startup: mode.startup,
      start_folder: mode.startFolder,
    });
    expect(inactiveRunMode(mode)).toEqual({ workload: mode.workload });
  });

  it("removing the startup harness clears the startup and never substitutes another", () => {
    const mode: RunModeDraft = { experience: "interactive", tools: [claude, codex], startup: { kind: "harness", tool: "claude-code" } };
    const removed = withoutTool(mode, "claude-code");
    expect(removed.clearedStartup).toBe(true);
    expect(removed.mode.startup).toBeUndefined();
    expect(removed.mode.tools).toEqual([codex]);
    const other = withoutTool(mode, "codex-cli");
    expect(other.clearedStartup).toBe(false);
    expect(other.mode.startup).toEqual(mode.startup);
    const command = withoutTool({ ...mode, startup: { kind: "command", command: "npm start" } }, "claude-code");
    expect(command.mode.startup).toEqual({ kind: "command", command: "npm start" });
  });

  it("the blocker is the server's own sentence and reason", () => {
    expect(runModeBlocker(undefined)).toEqual({ reason: RUN_MODE_REASON.REQUIRED, text: RUN_MODE_REFUSAL.RUN_MODE_REQUIRED() });
    expect(runModeBlocker({ experience: "background", tools: [] })).toEqual({
      reason: RUN_MODE_REASON.REQUIRED,
      text: RUN_MODE_REFUSAL.RUN_WORKLOAD_REQUIRED(),
    });
    expect(runModeBlocker({ experience: "interactive", tools: [claude], startup: { kind: "harness", tool: "codex-cli" } })).toEqual({
      reason: RUN_MODE_REASON.CONFLICT,
      text: RUN_MODE_REFUSAL.STARTUP_TOOL_UNKNOWN("codex-cli"),
    });
    expect(runModeBlocker({ experience: "interactive", tools: [claude], startup: { kind: "harness", tool: "claude-code" } })).toBeNull();
    // A startup kept for the other mode does not block this one.
    expect(runModeBlocker({ experience: "background", tools: [], workload: { kind: "command", command: "x" }, startup: { kind: "harness", tool: "x" } })).toBeNull();
  });

  it("the contract wire carries the mode beside the runner and overrides", () => {
    const draft = emptyRunContractDraft();
    draft.runner.placement = "remote";
    draft.mode = { experience: "interactive", tools: [claude] };
    expect(buildRunContractWire(draft, NO_ACTIVE_SECTIONS)).toEqual({ placement: "remote", experience: "interactive", tools: [claude] });
  });

  it("maps an older client's input the way the server does", () => {
    expect(effectiveExperience({ task: "fix" })).toBe("background");
    expect(effectiveExperience({ task: "fix", interactive: true })).toBe("interactive");
    expect(effectiveExperience({})).toBe("interactive");
    expect(effectiveExperience({ task: "  " })).toBe("interactive");
    expect(effectiveExperience({ experience: "background", interactive: true })).toBe("background");
  });

  it("push rules are keyed by provider and the organisation in lower case, as on the server", () => {
    expect(pushRuleKey({ provider: "github", org: "Acme" })).toBe("github/acme");
  });
});
