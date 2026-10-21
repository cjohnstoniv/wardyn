/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runner-pools?run_type= per scenario, typed by the wire twin. The builders are exported so a
// test composes one pool state (switched off, pool full, no KVM) from the scenario's own rows.

import type {
  BarrierOption, EffectiveLimit, EffectiveRunLimits, LimitSource, RunnerPoolCatalogueEntry, RunnerPoolList, RunnerPoolRunType,
} from "../../types/runner-pools";
import type { ConfinementClass } from "../../types/runs";
import { everyScenario, type ByScenario } from "./index";

const limit = (cap: number, source: LimitSource | undefined, def = cap): EffectiveLimit => ({
  default: { value: def }, cap: { value: cap }, ...(source ? { cap_source: source } : {}),
});
export const NO_LIMIT: EffectiveLimit = { default: { value: 0, unlimited: true }, cap: { value: 0, unlimited: true } };

/** One run type's effective limits as the pool list answers them: a 4 CPU, 8 GiB, 24 hour pool. */
export const effectiveLimits = (runType: RunnerPoolRunType, over: Partial<EffectiveRunLimits> = {}): EffectiveRunLimits => ({
  run_type: runType,
  cpu_millis: limit(4000, "pool", 2000),
  memory_mib: limit(8192, "pool", 4096),
  disk_mib: limit(20480, "pool"),
  lifetime_sec: limit(runType === "interactive" ? 3600 : 86400, "pool"),
  lifetime_span_sec: limit(runType === "interactive" ? 3600 : 86400, "pool"),
  lifetime_lease_sec: NO_LIMIT,
  no_end_allowed: false,
  concurrency: [],
  ...over,
});

/** Always CC1, CC2, CC3 in that order; the server's default is Wall unless a row overrides it. */
export const barrierOptions = (over: Partial<Record<ConfinementClass, Partial<BarrierOption>>> = {}): BarrierOption[] =>
  (["CC1", "CC2", "CC3"] as const).map((cc) => ({
    class: cc, available: true, ...(cc === "CC2" ? { default: true, default_source: "server" as const } : {}), ...over[cc],
  }));

export const SHARED: RunnerPoolCatalogueEntry = {
  id: "c1111111-1111-4111-8111-111111111111", name: "Shared pool", scope: "organization", availability: "available",
  limits: [effectiveLimits("background"), effectiveLimits("interactive")],
  run_types: ["interactive", "background"], run_types_source: "pool",
  barrier_options: barrierOptions(), slots: { used: 2, waiting: 0, max: 10, verdict: "starts_now", max_wait_sec: 86400 },
};
export const TEAM_LAPTOPS: RunnerPoolCatalogueEntry = {
  id: "c2222222-2222-4222-8222-222222222222", name: "Team laptops", scope: "user", availability: "available",
  limits: [effectiveLimits("background", { cpu_millis: limit(2000, "runner") }), effectiveLimits("interactive")],
  run_types: ["interactive", "background"], run_types_source: "none",
  barrier_options: barrierOptions({ CC3: { available: false, reason_code: "not_offered", reason_detail: "kvm_missing" } }),
  my_runners: [{ id: "d1111111-1111-4111-8111-111111111111", name: "Laptop", online: true }],
  slots: { used: 0, waiting: 0, verdict: "starts_now" },
};
/** A pool the caller may use but that cannot take this run: the row stays, disabled, with its reason. */
export const unavailablePool = (pool: RunnerPoolCatalogueEntry, reason: NonNullable<RunnerPoolCatalogueEntry["reason"]>): RunnerPoolCatalogueEntry => ({
  ...pool, availability: "unavailable", reason,
});

const MEMBER: RunnerPoolList = {
  pools: [SHARED], allowance: { active: 1, max: 5 }, default: { pool_id: SHARED.id, selection: "organisation_default" },
};
const UNKNOWN_BARRIERS: BarrierOption[] = (["CC1", "CC2", "CC3"] as const).map((cc) => ({ class: cc, available: false, reason_code: "unknown" }));

/** GET /runner-pools, per scenario. */
export const SCENARIO_POOLS: ByScenario<RunnerPoolList> = everyScenario(MEMBER, {
  governed: {
    pools: [{ ...SHARED, barrier_options: barrierOptions({
      CC1: { available: false, reason_code: "below_floor", floor: "CC2" },
      CC2: { default: true, default_source: "organisation" },
    }) }],
    allowance: { active: 2, max: 5, unit_name: "Platform team" },
    default: { pool_id: SHARED.id, selection: "organisation_default" },
  },
  full: {
    ...MEMBER, pools: [{ ...SHARED, slots: { used: 10, waiting: 3, max: 10, verdict: "waits", max_wait_sec: 86400 } }],
    allowance: { active: 5, max: 5 },
  },
  offline: {
    pools: [{
      ...unavailablePool(TEAM_LAPTOPS, "runner_pool_no_online_runner"),
      my_runners: [{ id: "d1111111-1111-4111-8111-111111111111", name: "Laptop", online: false }], barrier_options: UNKNOWN_BARRIERS,
    }],
    default: {},
  },
  noKvm: { ...MEMBER, pools: [{ ...SHARED, barrier_options: barrierOptions({ CC3: { available: false, reason_code: "not_offered", reason_detail: "kvm_missing" } }) }] },
  admin: { pools: [SHARED, TEAM_LAPTOPS], default: { pool_id: TEAM_LAPTOPS.id, selection: "personal_default" } },
  interactive2: { ...MEMBER, pools: [SHARED, unavailablePool({ ...TEAM_LAPTOPS, run_types: ["background"] }, "runner_pool_run_type_not_allowed")] },
});
