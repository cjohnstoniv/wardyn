/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wire facts Review reads that the TS twins do not carry yet. Each is typed
// here as the design draws it, so the day its twin lands this file shrinks to
// an import and no group changes. Every field is optional: absent means the
// server did not say, and Review then shows less, never a guess.

import type { PreflightResult } from "../../../../lib/types/preflight";
import type { RequestedResources } from "../../../../lib/types/new-run-contract";
import type { ConfinementClass } from "../../../../lib/types/runs";
import type { RunnerPoolChoice } from "../../../../lib/types/runner-pools";
import type { RunIntent } from "../draft/run-intent";

/** Seam: Q-c. `runner_pool_slots` on preflight and preview (client.PoolSlots). */
export interface PoolSlotsFact {
  used: number;
  /** Absent: the pool has no limit. */
  max?: number;
  waiting: number;
  verdict: "starts_now" | "waits";
  max_wait_sec?: number;
}

/** Seam: DS-5. One of the caller's own runners in a self-hosted pool. */
export interface PoolRunnerFact {
  id: string;
  name: string;
  online: boolean;
}

/** Seam: DS-3. What the main drive's size does on the resolved runner. */
export interface MainDriveFact {
  enforcement: "filesystem" | "eviction" | "none" | "unknown";
}

/** Seam: DS-5. The resolved pool's barrier options; the default says whether the organisation or the server picked it. */
export interface BarrierOptionFact {
  class: ConfinementClass;
  available: boolean;
  default?: boolean;
  default_source?: "organisation" | "server";
}

export type ReviewPoolChoice = RunnerPoolChoice & { my_runners?: PoolRunnerFact[] };

export type ReviewPreflight = Omit<PreflightResult, "runner_pools"> & {
  runner_pools?: ReviewPoolChoice[];
  runner_pool_slots?: PoolSlotsFact;
  barrier_options?: BarrierOptionFact[];
  main_drive?: MainDriveFact;
};

/** Seam: DS-3. The main drive's size in whole GiB, written by the Files step as `resources.disk_mib`. */
export const mainDriveGiB = (intent: RunIntent): number | undefined => {
  const mib = (intent.resources as (RequestedResources & { disk_mib?: number }) | undefined)?.disk_mib;
  return mib === undefined ? undefined : mib / 1024;
};

/** The caller's own runners that are online in the resolved pool. */
export function onlineRunners(pre: ReviewPreflight | null): number | undefined {
  const pool = pre?.runner_pools?.find((p) => p.id === pre.runner_pool?.id);
  return pool?.my_runners ? pool.my_runners.filter((r) => r.online).length : undefined;
}
