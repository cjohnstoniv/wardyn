/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns experience, workload.kind, runner_pool_id and confinement_class (a command and the ports
// of an application server are apps-lens).

import type { ConfinementClass } from "../../../../lib/types/runs";
import type { RunnerPoolScope } from "../../../../lib/types/runner-pools";
import type { RunIntent } from "../draft/run-intent";
import { appIssues, commandOf } from "./apps-lens";
import { withKeys } from "./lens-keys";
import { shapeCarriers, type RunMode } from "./shape-lens";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

export function chooseMode(intent: RunIntent, mode: RunMode): RunIntent {
  if (intent.shape) return withKeys(intent, shapeCarriers(intent.shape, mode, intent));
  return withKeys(intent, { experience: mode === "interactive" ? "interactive" : "background" });
}

/** Organization pools and User pools: the wire's scope is the console's word for the two kinds of pool. */
export type PoolScope = RunnerPoolScope;
export const poolScope = (scope: RunnerPoolScope): PoolScope => scope;

export const choosePool = (intent: RunIntent, poolId: string): RunIntent => withKeys(intent, { runner_pool_id: poolId });

/** Called only when the person changes the barrier: an untouched draft lets the server choose. */
export const chooseBarrier = (intent: RunIntent, cc: ConfinementClass): RunIntent => withKeys(intent, { confinement_class: cc });

export const runIssues: StepValidator = (intent, ctx) => {
  const out: StepIssue[] = [];
  const command = intent.shape === "application" || intent.workload?.kind === "command";
  if (command && !commandOf(intent)?.trim()) out.push(issue("run", "command_required"));
  if (!intent.runner_pool_id && ctx.poolDefault === false) out.push(issue("run", "pool_required"));
  return [...out, ...appIssues(intent)];
};
