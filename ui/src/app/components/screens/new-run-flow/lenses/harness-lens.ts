/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns tools[kind=harness], startup, workload.agent and workload.task.

import type { RunIntent } from "../draft/run-intent";
import { harnessIds, withKeys } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

/** A harness added beside the older tool-approval hold starts held, so a clone is never quieter. */
export function addHarness(intent: RunIntent, id: string): RunIntent {
  if (harnessIds(intent).includes(id)) return intent;
  const held = intent.tool_approvals === "hold" ? { default_effect: "hold" as const } : {};
  const tools = [...(intent.tools ?? []), { id, kind: "harness" as const, ...held }];
  const workload = intent.workload?.kind === "agent_task" && !intent.workload.agent ? { ...intent.workload, agent: id } : intent.workload;
  return withKeys(intent, { tools, workload });
}

/** Never removes an agent task's last harness. */
export function removeHarness(intent: RunIntent, id: string): RunIntent {
  const ids = harnessIds(intent);
  if (!ids.includes(id) || (intent.shape === "agent_task" && ids.length === 1)) return intent;
  const next = ids.find((h) => h !== id);
  const tools = (intent.tools ?? []).filter((t) => !(t.kind === "harness" && t.id === id));
  const workload = intent.workload?.agent === id ? { ...intent.workload, agent: next } : intent.workload;
  const startup = intent.startup?.kind === "harness" && intent.startup.tool === id ? undefined : intent.startup;
  return withKeys(intent, { tools, workload, startup });
}

export const setStarter = (intent: RunIntent, id: string): RunIntent => withKeys(intent, { startup: { kind: "harness", tool: id } });

export function setTask(intent: RunIntent, task: string): RunIntent {
  const agent = intent.workload?.agent ?? harnessIds(intent)[0];
  return withKeys(intent, { workload: { kind: "agent_task", ...(agent ? { agent } : {}), task } });
}

export const harnessIssues: StepValidator = (intent) => {
  if (intent.shape !== "agent_task") return [];
  const out: StepIssue[] = [];
  if (!harnessIds(intent).length) out.push(issue("harnesses", "harness_required"));
  if (intent.experience === "background" && !intent.workload?.task?.trim()) out.push(issue("harnesses", "task_required"));
  return out;
};
