/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Helpers over the wire facts Review reads; the types live in lib/types/new-run-review.

import type { RequestedResources } from "../../../../lib/types/new-run-contract";
import type { ReviewPreflight } from "../../../../lib/types/new-run-review";
import type { RunIntent } from "../draft/run-intent";

export type { BarrierOptionFact, PoolRunnerFact, PoolSlotsFact, ReviewPoolChoice, ReviewPreflight } from "../../../../lib/types/new-run-review";

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
