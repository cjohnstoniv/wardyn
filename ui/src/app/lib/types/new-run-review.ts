/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wire facts the New Run Review reads that the TS twins do not carry yet. Each
// is typed as the design draws it, so the day its twin lands this file shrinks
// to an import. Every field is optional: absent means the server did not say,
// and Review then shows less, never a guess.

import type { PreflightResult } from "./preflight";
import type { ConfinementClass } from "./runs";
import type { PoolSlots, RunnerPoolChoice } from "./runner-pools";

/** `runner_pool_slots` on preflight and preview (client.PoolSlots). */
export type PoolSlotsFact = PoolSlots;

/** Seam: DS-5. One of the caller's own runners in a self-hosted pool. */
export interface PoolRunnerFact {
  id: string;
  name: string;
  online: boolean;
}

/** Seam: DS-3 §3.3. What the resolved pool's runners do with the main drive's size. */
export interface MainDriveFacts {
  enforcement: "filesystem" | "eviction" | "none" | "unknown";
}

/** Seam: DS-3 §3.5 (client.MainDriveApplied). `applied_mib` 0 means no size; enforcement is under `facts`. */
export interface MainDriveApplied {
  requested_mib?: number;
  applied_mib: number;
  basis?: "request" | "policy" | "default" | "none";
  default_source?: "pool" | "deployment";
  clamped_by?: "pool" | "governance" | "deployment" | "policy";
  soft?: boolean;
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

export type ReviewPreflight = Omit<PreflightResult, "runner_pools"> & {
  runner_pools?: ReviewPoolChoice[];
  barrier_options?: BarrierOptionFact[];
  main_drive?: MainDriveApplied;
};
