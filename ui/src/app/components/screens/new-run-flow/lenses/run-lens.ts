/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns experience, workload.kind, workload.command, runner_pool_id,
// machine_kind and confinement_class.

import type { ConfinementClass } from "../../../../lib/types/runs";
import type { RunnerPoolScope } from "../../../../lib/types/runner-pools";
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

/** Organization pools and User pools: the wire's scope is the console's word for the two kinds of pool. */
export type PoolScope = RunnerPoolScope;
export const poolScope = (scope: RunnerPoolScope): PoolScope => scope;

export const choosePool = (intent: RunIntent, poolId: string): RunIntent => withKeys(intent, { runner_pool_id: poolId });

/**
 * The machine kind. A container is the default, so it is the absent key; a VM is its own barrier, so a
 * barrier chosen for a container is dropped with the switch.
 */
export const chooseMachineKind = (intent: RunIntent, kind: "container" | "linux_vm"): RunIntent =>
  kind === "linux_vm" ? withKeys(intent, { machine_kind: "linux_vm", confinement_class: undefined }) : withKeys(intent, { machine_kind: undefined });

/** Called only when the person changes the barrier: an untouched draft lets the server choose. */
export const chooseBarrier = (intent: RunIntent, cc: ConfinementClass): RunIntent => withKeys(intent, { confinement_class: cc });

export const runIssues: StepValidator = (intent, ctx) => {
  const out: StepIssue[] = [];
  const command = intent.shape === "application" || intent.workload?.kind === "command";
  if (command && !intent.workload?.command?.trim()) out.push(issue("run", "command_required"));
  if (!intent.runner_pool_id && ctx.poolDefault === false) out.push(issue("run", "pool_required"));
  return out;
};
