/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns tools[kind=harness], startup, workload.agent and workload.task.

import type { IncludedTool } from "../../../../lib/types/new-run-contract";
import type { RunIntent } from "../draft/run-intent";
import { harnessIds, withKeys } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

const autonomous = (intent: RunIntent) => intent.shape === "agent_task" && intent.experience === "background";

/** A harness added beside the older tool-approval hold starts held, so a clone is never quieter. */
const harnessTool = (intent: RunIntent, id: string): IncludedTool =>
  ({ id, kind: "harness", ...(intent.tool_approvals === "hold" ? { default_effect: "hold" as const } : {}) });

/** An autonomous run holds one harness: a second is refused here, whatever the roster allows. */
export function addHarness(intent: RunIntent, id: string): RunIntent {
  const ids = harnessIds(intent);
  if (ids.includes(id) || (autonomous(intent) && ids.length > 0)) return intent;
  const tools = [...(intent.tools ?? []), harnessTool(intent, id)];
  const workload = intent.workload?.kind === "agent_task" && !intent.workload.agent ? { ...intent.workload, agent: id } : intent.workload;
  return withKeys(intent, { tools, workload });
}

/** Swaps one harness for another in place; the card keeps its task or starting prompt, the old harness's tool settings go. */
export function swapHarness(intent: RunIntent, from: string, to: string): RunIntent {
  if (from === to || !harnessIds(intent).includes(from) || harnessIds(intent).includes(to)) return intent;
  const tools = (intent.tools ?? []).map((t) => (t.kind === "harness" && t.id === from ? harnessTool(intent, to) : t));
  const workload = intent.workload?.agent === from ? { ...intent.workload, agent: to } : intent.workload;
  const startup = intent.startup?.kind === "harness" && intent.startup.tool === from ? { ...intent.startup, tool: to } : intent.startup;
  return withKeys(intent, { tools, workload, startup });
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
