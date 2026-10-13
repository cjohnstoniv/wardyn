/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wire facts the New Run Review reads that the TS twins do not carry yet. Each
// is typed as the design draws it, so the day its twin lands this file shrinks
// to an import. Every field is optional: absent means the server did not say,
// and Review then shows less, never a guess.

import type { MainDriveApplied as RealMainDriveApplied } from "./new-run-contract";
import type { PreflightResult } from "./preflight";
import type { ConfinementClass } from "./runs";
import type { PoolSlots, RunnerPoolChoice } from "./runner-pools";

/** `runner_pool_slots` on preflight and preview (client.PoolSlots). */
export type PoolSlotsFact = PoolSlots;

/** Seam: DS-5. One of the caller's own runners in a user pool. */
export interface PoolRunnerFact {
  id: string;
  name: string;
  online: boolean;
}

/** Seam: DS-3 §3.3. What the resolved pool's runners do with the main drive's size. */
export interface MainDriveFacts {
  enforcement: "filesystem" | "eviction" | "none" | "unknown";
}

/** The twin landed (new-run-contract.ts); only the pool's facts (DS-3 §3.3) are still a seam. */
export interface MainDriveApplied extends RealMainDriveApplied {
  facts?: MainDriveFacts;
}

/** Seam: DS-5. The resolved pool's barrier options; the default says whether the organisation or the server picked it. */
export interface BarrierOptionFact {
  class: ConfinementClass;
  available: boolean;
  default?: boolean;
  default_source?: "organisation" | "server";
}

export type ReviewPoolChoice = RunnerPoolChoice & { my_runners?: PoolRunnerFact[] };

export type ReviewPreflight = Omit<PreflightResult, "runner_pools" > & {
  runner_pools?: ReviewPoolChoice[];
  barrier_options?: BarrierOptionFact[];
  main_drive?: MainDriveApplied;
};
