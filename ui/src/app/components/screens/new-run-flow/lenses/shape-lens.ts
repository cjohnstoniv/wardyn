/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `shape`, and keeps the run-mode carriers coherent with it (DS-5's four
// draft rows; run-mode-map.ts holds only the older fields' adapter).

import { isVMKind } from "../../../../lib/types/machine-kind";
import type { RunShape } from "../../../../lib/types/runs";
import { withAskedRunMode } from "../document/legacy-run-mode";
import type { RunIntent } from "../draft/run-intent";
import { isServer, setApps } from "./apps-lens";
import { dropAgentTasks, harnessIds, withKeys } from "./lens-keys";
import { issue, type StepValidator } from "./step-issues";

export type RunMode = "interactive" | "autonomous";

export const modeOf = (intent: RunIntent): RunMode | undefined =>
  intent.experience === "interactive" ? "interactive" : intent.experience === "background" ? "autonomous" : undefined;

/** The carriers one shape and mode say. Undefined removes the key. */
export function shapeCarriers(shape: RunShape, mode: RunMode | undefined, intent: RunIntent): Pick<RunIntent, "experience" | "workload" | "startup" | "tools"> {
  const ids = harnessIds(intent);
  const started = intent.startup?.kind === "harness" && ids.includes(intent.startup.tool ?? "") ? intent.startup : undefined;
  // The text belongs to one agent: the run's task to workload.agent while it is among the harnesses, a first message
  // to the started harness. That agent is the one the other mode runs or starts, so text never moves to another agent.
  const taskAgent = ids.includes(intent.workload?.agent ?? "") ? intent.workload?.agent : started?.tool ?? ids[0];
  // The person's text crosses a mode switch: the task is the started harness's first message, and back. A blank text is
  // none, and Your own tools starts no CLI, so it takes no prompt.
  const said = started && started.tool === taskAgent ? started.prompt : undefined;
  const prompt = taskAgent !== "none" && intent.workload?.task?.trim() ? { prompt: intent.workload.task } : {};
  // A VM's terminal opens a shell, not the agent, and the server refuses a harness start there.
  const startup = isVMKind(intent.machine_kind) ? undefined : started ?? (taskAgent ? { kind: "harness" as const, tool: taskAgent, ...prompt } : undefined);
  switch (shape) {
    case "environment":
      return { experience: "interactive", workload: undefined, startup: undefined, ...dropAgentTasks(intent) };
    case "application":
      return { experience: "background", workload: { kind: "command", command: intent.workload?.command ?? "" }, startup: undefined, ...dropAgentTasks(intent) };
    case "agent_task":
      if (mode === "autonomous") {
        return {
          experience: "background",
          workload: { kind: "agent_task", ...(taskAgent ? { agent: taskAgent } : {}), task: intent.workload?.task || (said?.trim() ? said : "") },
          startup: undefined,
        };
      }
      if (mode === "interactive") {
        return { experience: "interactive", workload: undefined, startup, ...dropAgentTasks(intent) };
      }
      return { experience: undefined, workload: undefined, startup: undefined, ...dropAgentTasks(intent) };
  }
}

/** What a shape says of a run in the older fields: an environment is interactive and an application runs a command. An agent task's mode is chosen on the Run step. */
const SHAPE_SAYS: Partial<Record<RunShape, Parameters<typeof withAskedRunMode>[1]>> = {
  environment: { interactive: true },
  application: { interactive: false, task_mode: "exec" },
};

export function chooseShape(intent: RunIntent, shape: RunShape): RunIntent {
  // A draft still in the older run-mode fields (a template that leaves its run mode to the person) is converted
  // with the mode the shape names; carriers converted to this very shape are the run as written.
  const says = SHAPE_SAYS[shape];
  const asked = says ? withAskedRunMode(intent, says) : intent;
  // The shape the draft already has: choosing it again changes nothing, so the starter, its prompt and the
  // task a document, a template or an earlier step set are still there after the workload press on Shape.
  if (asked === intent && intent.shape === shape) return intent;
  // A kept or resumed machine belongs to an interactive run: a shape that is background takes it along. A shape whose mode is
  // not chosen yet (an agent task) leaves it; the Run step's mode choice, and the server, say the rest.
  const next = asked !== intent && asked.shape === shape ? asked : withKeys(asked, { shape, ...shapeCarriers(shape, undefined, asked) });
  const kept = next.machine && next.experience === "background" ? withKeys(next, { machine: undefined }) : next;
  // The ports belong to the server card: leaving it takes them along.
  return isServer(intent) ? setApps(kept, []) : kept;
}

export const shapeIssues: StepValidator = (intent) => (intent.shape ? [] : [issue("shape", "shape_required")]);
