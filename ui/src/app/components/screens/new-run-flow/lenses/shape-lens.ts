/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `shape`, and keeps the run-mode carriers coherent with it (DS-5's four
// draft rows; run-mode-map.ts holds only the older fields' adapter).

import type { RunShape } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
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
  switch (shape) {
    case "environment":
      return { experience: "interactive", workload: undefined, startup: undefined, ...dropAgentTasks(intent) };
    case "application":
      return { experience: "background", workload: { kind: "command", command: intent.workload?.command ?? "" }, startup: undefined, ...dropAgentTasks(intent) };
    case "agent_task":
      if (mode === "autonomous") {
        return {
          experience: "background",
          workload: { kind: "agent_task", ...(taskAgent ? { agent: taskAgent } : {}), task: intent.workload?.task ?? "" },
          startup: undefined,
        };
      }
      if (mode === "interactive") {
        return { experience: "interactive", workload: undefined, startup: harness ? { kind: "harness", tool: harness } : undefined, ...dropAgentTasks(intent) };
      }
      return { experience: undefined, workload: undefined, startup: undefined, ...dropAgentTasks(intent) };
  }
}

export function chooseShape(intent: RunIntent, shape: RunShape): RunIntent {
  // The shape the draft already has: choosing it again changes nothing, so the starter, its prompt and the
  // task a document, a template or an earlier step set are still there after the workload press on Shape.
  if (intent.shape === shape) return intent;
  return withKeys(intent, { shape, ...shapeCarriers(shape, undefined, intent) });
}

export const shapeIssues: StepValidator = (intent) => (intent.shape ? [] : [issue("shape", "shape_required")]);
