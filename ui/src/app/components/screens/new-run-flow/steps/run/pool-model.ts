/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Run step reads off GET /runner-pools?run_type=: which pools are listed and why one is
// disabled, which row is selected, the per-run limits of the chosen run type, and the barrier rows.
// Every sentence is a canon string or a server fact; nothing is guessed when a fact is absent.

import { CC_ORDER, type ConfinementClass } from "../../../../../lib/types/runs";
import type {
  BarrierOption as WireBarrier, EffectiveLimit, EffectiveRunLimits, LimitSource, PersonAllowance, RunnerPoolCatalogueEntry,
  RunnerPoolList, RunnerPoolRunType,
} from "../../../../../lib/types/runner-pools";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { BarrierOption } from "../../../../wardyn/barrier-compare-table";
import type { RunIntent } from "../../draft/run-intent";
import { mibToGibText } from "../../lenses/files-lens";
import { cpuText, lifetimeText, memoryText } from "../../review/format";

/** Pools the caller may see: a row they may only manage (runner_pool_not_found) is not a choice. */
export const usablePools = (list: RunnerPoolList): RunnerPoolCatalogueEntry[] =>
  list.pools.filter((p) => p.reason !== "runner_pool_not_found");

/** The short label after "{name} · " on a disabled option (DS-5 OQ-2). */
export function reasonLabel(pool: RunnerPoolCatalogueEntry, asked: RunnerPoolRunType | undefined): string {
  switch (pool.reason) {
    case "runner_pool_unavailable": return NR_RUN.REASON_SWITCHED_OFF;
    case "placement_denied": return NR_RUN.REASON_NOT_ALLOWED_PROFILE;
    case "placement_unavailable": return NR_RUN.REASON_RUNNERS_OFF;
    case "runner_pool_no_online_runner": return NR_RUN.REASON_NO_ONLINE;
    case "runner_pool_run_type_not_allowed": return asked === "background" ? NR_RUN.REASON_INTERACTIVE_ONLY : NR_RUN.REASON_BACKGROUND_ONLY;
    default: return NR_RUN.REASON_NO_RUNNERS;
  }
}

export interface PoolChoice {
  id: string;
  label: string;
  disabled: boolean;
}

/** Every online runner in the pool already holds its Runs at once, so a new run waits for room. */
export function poolHasNoRoom(pool: RunnerPoolCatalogueEntry): boolean {
  const c = pool.capacity;
  return !!c && c.runners_online > 0 && c.total.runs > 0 && c.available.runs === 0;
}

/**
 * Whether a run launched now starts in this pool or waits: it waits when the pool's slots say so, or when
 * its runners have no room. Undefined when the list carries neither fact.
 */
export function fitVerdict(pool: RunnerPoolCatalogueEntry): string | undefined {
  if (pool.slots?.verdict === "waits" || poolHasNoRoom(pool)) return NR_RUN.FIT_WAITS;
  return pool.slots ? NR_RUN.FIT_STARTS_NOW : undefined;
}

export const poolChoices = (pools: readonly RunnerPoolCatalogueEntry[], asked: RunnerPoolRunType | undefined): PoolChoice[] =>
  pools.map((p) => {
    if (p.availability !== "available") return { id: p.id, label: NR_RUN.POOL_OPTION_REASON(p.name, reasonLabel(p, asked)), disabled: true };
    const fit = fitVerdict(p);
    return { id: p.id, label: fit ? NR_RUN.POOL_OPTION_REASON(p.name, fit) : p.name, disabled: false };
  });

/** The draft's pool if it has one, else the pool an untouched run resolves to; "" when neither is listed. */
export function selectedPoolId(intent: RunIntent, list: RunnerPoolList): string {
  const id = intent.runner_pool_id ?? list.default?.pool_id;
  return id && usablePools(list).some((p) => p.id === id) ? id : "";
}

/** The lede names whose defaults are selected (pending W-05). */
export function ledeOf(list: RunnerPoolList | undefined): string {
  if (list?.default?.selection === "organisation_default") return NR_RUN.LEDE_ORG;
  return list?.default?.selection === "personal_default" ? NR_RUN.LEDE_PERSONAL : NR_RUN.LEDE_NONE;
}

/** Who bounds the limits: a named profile, an unnamed organisation, or nobody but the server (C3). */
export function limitsNote(allowance: PersonAllowance | undefined): string {
  if (!allowance) return NR_RUN.LIMITS_NOTE_UNGOVERNED;
  return allowance.profile_name ? NR_RUN.LIMITS_NOTE(allowance.profile_name) : NR_RUN.LIMITS_NOTE_ORG;
}

/** "Set by ..." for one cell. A deployment bound has no canon sentence yet, so it says nothing. */
export function sourceText(source: LimitSource | undefined, allowance: PersonAllowance | undefined): string | undefined {
  switch (source) {
    case "pool": return NR_RUN.SET_BY_POOL;
    case "runner": return NR_RUN.SET_BY_RUNNERS;
    case "governance": return allowance?.profile_name ? NR_RUN.SET_BY(allowance.profile_name) : NR_RUN.SET_BY_ORG;
    default: return undefined;
  }
}

export interface LimitCell {
  key: "cpu" | "memory" | "lifetime" | "drive";
  label: string;
  value: string;
  note: string;
  source?: string;
}

/** The limits of the run type asked for; with none asked (a draft with no mode yet) the pool's first. */
export const limitsFor = (pool: RunnerPoolCatalogueEntry, asked: RunnerPoolRunType | undefined): EffectiveRunLimits | undefined =>
  pool.limits?.find((l) => l.run_type === asked) ?? (asked ? undefined : pool.limits?.[0]);

export function limitCells(limits: EffectiveRunLimits, allowance: PersonAllowance | undefined): LimitCell[] {
  const cap = (l: EffectiveLimit, text: (n: number) => string) => (l.cap.unlimited ? NR_RUN.LIMIT_NO_MAXIMUM : text(l.cap.value));
  const cell = (key: LimitCell["key"], label: string, l: EffectiveLimit, text: (n: number) => string, note: string = NR_RUN.LIMIT_MAXIMUM): LimitCell =>
    ({ key, label, value: cap(l, text), note, source: sourceText(l.cap_source, allowance) });
  return [
    cell("cpu", NR_RUN.LIMIT_CPU, limits.cpu_millis, cpuText),
    cell("memory", NR_RUN.LIMIT_MEMORY, limits.memory_mib, memoryText),
    cell("lifetime", NR_RUN.LIMIT_LIFETIME, limits.lifetime_sec, lifetimeText),
    cell("drive", NR_RUN.LIMIT_MAIN_DRIVE, limits.disk_mib, (mib) => NR_SHARED.GIB(mibToGibText(mib, "floor")), NR_RUN.LIMIT_EXTRA_DRIVES),
  ];
}

export function runTypesText(pool: RunnerPoolCatalogueEntry): string {
  const types = pool.run_types;
  if (types.length === 1) return types[0] === "background" ? NR_RUN.RUN_TYPES_BACKGROUND : NR_RUN.RUN_TYPES_INTERACTIVE;
  return NR_RUN.RUN_TYPES_BOTH;
}

const BARRIER_NAME: Record<ConfinementClass, string> = { CC1: NR_SHARED.BARRIER_FENCE, CC2: NR_SHARED.BARRIER_WALL, CC3: NR_SHARED.BARRIER_VAULT };

/** The sentence for why a barrier cannot be picked; one reason code, one sentence (DS-5 §8). */
function barrierReason(option: WireBarrier, pool: string): string {
  const floor = BARRIER_NAME[option.floor ?? option.class];
  switch (option.reason_code) {
    case "below_floor": return NR_RUN.BARRIER_REQUIRED(floor);
    case "policy_floor": return NR_RUN.BARRIER_POLICY_REQUIRES(floor);
    case "access_floor": return NR_RUN.BARRIER_ACCESS_VAULT;
    case "pool_not_allowed": return NR_RUN.BARRIER_POOL_DISALLOWS(pool, BARRIER_NAME[option.class]);
    case "not_offered": return option.reason_detail === "kvm_missing" ? NR_RUN.BARRIER_NO_KVM : NR_RUN.BARRIER_NONE_OFFERS(BARRIER_NAME[option.class]);
    case "unknown": return NR_RUN.BARRIER_KNOWN_WHEN_ONLINE;
    default: return NR_RUN.BARRIER_UNKNOWN;
  }
}

export interface BarrierRows {
  options: BarrierOption[];
  /** The server's pick and who made it; absent when no barrier is offered. */
  pick?: { cc: ConfinementClass; badge: string };
}

/** Always three rows. With no pool answer yet, or a pool that offers none, each is disabled and says it is not known. */
export function barrierRows(pool: RunnerPoolCatalogueEntry | undefined): BarrierRows {
  const wire = pool?.barrier_options ?? [];
  const options = CC_ORDER.map((cc): BarrierOption => {
    const o = wire.find((w) => w.class === cc);
    if (!o) return { cc, available: false, reason: NR_RUN.BARRIER_UNKNOWN };
    return o.available ? { cc, available: true } : { cc, available: false, reason: barrierReason(o, pool?.name ?? "") };
  });
  const pick = wire.find((w) => w.available && w.default);
  return {
    options,
    pick: pick && { cc: pick.class, badge: pick.default_source === "organisation" ? NR_RUN.BADGE_ORG_DEFAULT : NR_RUN.BADGE_DEFAULT },
  };
}
