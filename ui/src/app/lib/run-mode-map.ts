/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one table from a run's shape to the 0.9 run-mode contract (pkg/client
// RunModeContractV09): the experience a person chose and the carriers that say
// it. The server never infers an experience, and a pool that lists run types
// refuses a run stored without one, so the console always names it.
import type { Experience, IncludedTool, RunStartup, RunWorkload } from "./types/new-run-contract";

/** The older run-mode fields the carriers replace; the server refuses a request that sends both. */
export interface LegacyRunMode {
  agent?: string;
  task?: string;
  interactive?: boolean;
  task_mode?: "harness" | "exec";
  interactive_start?: "shell" | "agent";
  seed_auto_tools?: boolean;
  tool_approvals?: "auto" | "hold";
  model_provider?: string;
}

export const LEGACY_RUN_MODE_FIELDS = [
  "agent", "task", "interactive", "task_mode", "interactive_start", "seed_auto_tools", "tool_approvals", "model_provider",
] as const satisfies readonly (keyof LegacyRunMode)[];

/** The contract marker and the carriers of one run. */
export interface RunModeCarriers {
  mode_contract: "0.9";
  experience: Experience;
  workload?: RunWorkload;
  tools?: IncludedTool[];
  startup?: RunStartup;
}

const harness = (id: string, modelProvider?: string): IncludedTool[] => [
  { id, kind: "harness", ...(modelProvider ? { model_provider: modelProvider } : {}) },
];

/**
 * The carriers for a run, or null when the contract cannot say it and the run keeps
 * its older fields: an interactive initial prompt (no carrier until startup.prompt),
 * tool approvals and the boot-seed opt-in (the server refuses both beside carriers),
 * a command that names an agent for want of an image (a command carries none), and a
 * prompt or command with no text.
 */
export function mapRunMode(run: LegacyRunMode): RunModeCarriers | null {
  if (run.seed_auto_tools || run.tool_approvals === "hold") return null;
  const text = run.task?.trim() ?? "";
  if (run.interactive) {
    if (!run.agent) return null;
    const tools = harness(run.agent, run.model_provider);
    if (run.interactive_start === "agent") {
      return text ? null : { mode_contract: "0.9", experience: "interactive", tools, startup: { kind: "harness", tool: run.agent } };
    }
    return { mode_contract: "0.9", experience: "interactive", tools, ...(text ? { startup: { kind: "command", command: run.task } } : {}) };
  }
  if (!text) return null;
  if (run.task_mode === "exec") {
    return run.agent ? null : { mode_contract: "0.9", experience: "background", workload: { kind: "command", command: run.task } };
  }
  if (!run.agent) return null;
  return {
    mode_contract: "0.9",
    experience: "background",
    workload: { kind: "agent_task", agent: run.agent, task: run.task },
    tools: harness(run.agent, run.model_provider),
  };
}
