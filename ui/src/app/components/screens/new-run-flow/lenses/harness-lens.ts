/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns tools[kind=harness], startup, workload.agent and workload.task.

import type { IncludedTool } from "../../../../lib/types/new-run-contract";
import type { RunIntent } from "../draft/run-intent";
import { agentTaskId, harnessIds, withKeys, withoutTask } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

const autonomous = (intent: RunIntent) => intent.shape === "agent_task" && intent.experience === "background";

/** A harness added beside the older tool-approval hold starts held, so a clone is never quieter. */
const harnessTool = (intent: RunIntent, id: string): IncludedTool =>
  ({ id, kind: "harness", ...(intent.tool_approvals === "hold" ? { default_effect: "hold" as const } : {}) });

/** An autonomous run holds one harness unless the server lets agents run side by side (`multi`): a second is refused here, whatever the roster allows. */
export function addHarness(intent: RunIntent, id: string, multi = false): RunIntent {
  const ids = harnessIds(intent);
  if (ids.includes(id) || (autonomous(intent) && ids.length > 0 && !multi)) return intent;
  const tools = [...(intent.tools ?? []), harnessTool(intent, id)];
  const workload = intent.workload?.kind === "agent_task" && !intent.workload.agent ? { ...intent.workload, agent: id } : intent.workload;
  return withKeys(intent, { tools, workload });
}

/** Swaps one harness for another in place; the card keeps its task or starting prompt, the old harness's tool settings go. */
export function swapHarness(intent: RunIntent, from: string, to: string): RunIntent {
  if (from === to || !harnessIds(intent).includes(from) || harnessIds(intent).includes(to)) return intent;
  const tools = (intent.tools ?? []).map((t) => (t.kind === "harness" && t.id === from ? { ...harnessTool(intent, to), ...(t.task ? { task: t.task } : {}) } : t));
  const workload = intent.workload?.agent === from ? { ...intent.workload, agent: to } : intent.workload;
  const startup = intent.startup?.kind === "harness" && intent.startup.tool === from ? { ...intent.startup, tool: to } : intent.startup;
  return withKeys(intent, { tools, workload, startup });
}

/** Never removes an agent task's last harness. */
export function removeHarness(intent: RunIntent, id: string): RunIntent {
  const ids = harnessIds(intent);
  if (!ids.includes(id) || (intent.shape === "agent_task" && ids.length === 1)) return intent;
  const next = ids.find((h) => h !== id);
  const kept = (intent.tools ?? []).filter((t) => !(t.kind === "harness" && t.id === id));
  // The run's task belongs to workload.agent. When that card goes, the next agent's own task moves up to be the run's.
  const hand = intent.workload?.agent === id && autonomous(intent);
  const tools = hand ? kept.map((t) => (t.id === next ? withoutTask(t) : t)) : kept;
  const workload = intent.workload?.agent === id
    ? { ...intent.workload, agent: next, ...(hand ? { task: kept.find((t) => t.id === next)?.task ?? "" } : {}) }
    : intent.workload;
  const startup = intent.startup?.kind === "harness" && intent.startup.tool === id ? undefined : intent.startup;
  return withKeys(intent, { tools, workload, startup });
}

/** The one harness the run starts in; choosing the one already starting keeps its prompt. */
export const setStarter = (intent: RunIntent, id: string): RunIntent =>
  intent.startup?.kind === "harness" && intent.startup.tool === id ? intent : withKeys(intent, { startup: { kind: "harness", tool: id } });

export const clearStarter = (intent: RunIntent): RunIntent => withKeys(intent, { startup: undefined });

/** The started harness's first message; empty removes it. Only a harness startup takes one. */
export function setStartPrompt(intent: RunIntent, prompt: string): RunIntent {
  if (intent.startup?.kind !== "harness") return intent;
  const startup = { ...intent.startup };
  delete startup.prompt;
  return withKeys(intent, { startup: prompt ? { ...startup, prompt } : startup });
}

const workloadAgent = (intent: RunIntent) => intent.workload?.agent ?? harnessIds(intent)[0];

/** One agent's task: the run's own for workload.agent, else the one its harness carries. */
export const agentTask = (intent: RunIntent, id: string): string =>
  id === workloadAgent(intent) ? intent.workload?.task ?? "" : (intent.tools ?? []).find((t) => t.kind === "harness" && t.id === id)?.task ?? "";

/** Sets one agent's task; empty removes a harness's own. */
export function setAgentTask(intent: RunIntent, id: string, task: string): RunIntent {
  if (id === workloadAgent(intent)) return setTask(intent, task);
  const tools = (intent.tools ?? []).map((t) => (t.kind === "harness" && t.id === id ? (task ? { ...t, task } : withoutTask(t)) : t));
  return withKeys(intent, { tools });
}

export function setTask(intent: RunIntent, task: string): RunIntent {
  const agent = intent.workload?.agent ?? harnessIds(intent)[0];
  return withKeys(intent, { workload: { kind: "agent_task", ...(agent ? { agent } : {}), task } });
}

export const harnessIssues: StepValidator = (intent) => {
  if (intent.shape !== "agent_task") return [];
  const out: StepIssue[] = [];
  if (!harnessIds(intent).length) out.push(issue("harnesses", "harness_required"));
  if (intent.experience !== "background") return out;
  const ids = harnessIds(intent);
  if (ids.length < 2) {
    if (!intent.workload?.task?.trim()) out.push(issue("harnesses", "task_required"));
    return out;
  }
  // Several agents: each one needs its own task, and a failed Continue lands on the first without.
  ids.forEach((id, index) => {
    if (!agentTask(intent, id).trim()) out.push({ ...issue("harnesses", "agent_task_required"), focus: agentTaskId(index) });
  });
  return out;
};
