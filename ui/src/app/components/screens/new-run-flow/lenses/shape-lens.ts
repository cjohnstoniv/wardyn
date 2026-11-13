/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `shape`, and keeps the run-mode carriers coherent with it (DS-5's four
// draft rows; run-mode-map.ts holds only the older fields' adapter).

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
  const harness = ids[0];
  // The run's task belongs to workload.agent, so it stays with that agent when it is still among the harnesses.
  const taskAgent = ids.includes(intent.workload?.agent ?? "") ? intent.workload?.agent : harness;
  // The person's text crosses a mode switch: the task is the started harness's first message, and back. Only the harness
  // the text belongs to takes it, a blank text is none, and Your own tools starts no CLI, so it takes no prompt.
  const said = intent.startup?.kind === "harness" && intent.startup.tool === taskAgent ? intent.startup.prompt : undefined;
  const prompt = taskAgent === harness && harness !== "none" && intent.workload?.task?.trim() ? { prompt: intent.workload.task } : {};
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
        return { experience: "interactive", workload: undefined, startup: harness ? { kind: "harness", tool: harness, ...prompt } : undefined, ...dropAgentTasks(intent) };
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
