/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns experience, workload.kind, workload.command, runner_pool_id and
// confinement_class.

import type { ConfinementClass } from "../../../../lib/types/runs";
import type { RunnerPoolHosting } from "../../../../lib/types/runner-pools";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { shapeCarriers, type RunMode } from "./shape-lens";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

export function chooseMode(intent: RunIntent, mode: RunMode): RunIntent {
  if (intent.shape) return withKeys(intent, shapeCarriers(intent.shape, mode, intent));
  return withKeys(intent, { experience: mode === "interactive" ? "interactive" : "background" });
}

export const setCommand = (intent: RunIntent, command: string): RunIntent =>
  withKeys(intent, { workload: { kind: "command", command } });

/**
 * Organization pools and User pools are the console's words for the two kinds of pool. Today's wire still
 * says hosting_type; this is the one place it is read, so the scope rename is a change to this map.
 */
export type PoolScope = "organization" | "user";
export const poolScope = (hosting: RunnerPoolHosting): PoolScope => (hosting === "self_hosted" ? "user" : "organization");

export const choosePool = (intent: RunIntent, poolId: string): RunIntent => withKeys(intent, { runner_pool_id: poolId });

/** Called only when the person changes the barrier: an untouched draft lets the server choose. */
export const chooseBarrier = (intent: RunIntent, cc: ConfinementClass): RunIntent => withKeys(intent, { confinement_class: cc });

export const runIssues: StepValidator = (intent, ctx) => {
  const out: StepIssue[] = [];
  const command = intent.shape === "application" || intent.workload?.kind === "command";
  if (command && !intent.workload?.command?.trim()) out.push(issue("run", "command_required"));
  if (!intent.runner_pool_id && ctx.poolDefault === false) out.push(issue("run", "pool_required"));
  return out;
};
