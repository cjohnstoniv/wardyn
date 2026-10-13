/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `shape`, and keeps the run-mode carriers coherent with it (DS-5's four
// draft rows; run-mode-map.ts holds only the older fields' adapter).

import type { RunShape } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
import { harnessIds, withKeys } from "./lens-keys";
import { issue, type StepValidator } from "./step-issues";

export type RunMode = "interactive" | "autonomous";

export const modeOf = (intent: RunIntent): RunMode | undefined =>
  intent.experience === "interactive" ? "interactive" : intent.experience === "background" ? "autonomous" : undefined;

/** The carriers one shape and mode say. Undefined removes the key. */
export function shapeCarriers(shape: RunShape, mode: RunMode | undefined, intent: RunIntent): Pick<RunIntent, "experience" | "workload" | "startup"> {
  const harness = harnessIds(intent)[0];
  switch (shape) {
    case "environment":
      return { experience: "interactive", workload: undefined, startup: undefined };
    case "application":
      return { experience: "background", workload: { kind: "command", command: intent.workload?.command ?? "" }, startup: undefined };
    case "agent_task":
      if (mode === "autonomous") {
        return {
          experience: "background",
          workload: { kind: "agent_task", ...(harness ? { agent: harness } : {}), task: intent.workload?.task ?? "" },
          startup: undefined,
        };
      }
      if (mode === "interactive") {
        return { experience: "interactive", workload: undefined, startup: harness ? { kind: "harness", tool: harness } : undefined };
      }
      return { experience: undefined, workload: undefined, startup: undefined };
  }
}

export function chooseShape(intent: RunIntent, shape: RunShape): RunIntent {
  const mode = shape === "agent_task" && intent.shape === "agent_task" ? modeOf(intent) : undefined;
  return withKeys(intent, { shape, ...shapeCarriers(shape, mode, intent) });
}

export const shapeIssues: StepValidator = (intent) => (intent.shape ? [] : [issue("shape", "shape_required")]);
