/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A runner's and a pool's resources as the console shows them (mock M-POOLS v6): Total, Per run (a pool:
// the biggest run it can take), In use and Available for CPU, memory, disk and runs. Every number is the
// server's: In use and Available come from usage, never from a subtraction here, and a pool's figures are
// the sums of its members' the way the server's own pool capacity adds them. Display units are CPUs and
// GB (the wire's MiB over 1024), one decimal at most.

import { BARRIER_LABEL } from "../../../lib/runner-pool-refusals";
import type { RemoteRunnerView, RunnerPool, RunnerPoolLimits, RunnerPoolMemberView, RunnerPoolRunType } from "../../../lib/types/runner-pools";
import type { RunnerAmounts, RunnerLimits, RunnerTally, RunnerUsage, RunnerView } from "../../../lib/types/runners";
import { POOL_DRAWER, POOLS, RES } from "../../wardyn/copy/runner-pools";

const round1 = (n: number) => Number(n.toFixed(1));
export const cpus = (millis: number) => round1(millis / 1000);
export const gb = (mib: number) => round1(mib / 1024);
export const amountsText = (a: RunnerAmounts) => RES.AMOUNTS(cpus(a.cpu_millis), gb(a.memory_mib), gb(a.disk_mib));

export type ResKey = "cpu" | "mem" | "disk" | "runs";
export const RES_KEYS: ResKey[] = ["cpu", "mem", "disk", "runs"];
export const RES_NAME: Record<ResKey, string> = { cpu: RES.CPU, mem: RES.MEMORY, disk: RES.DISK, runs: RES.RUNS };

/** One resource in display units; perRun is null for runs. */
export interface ResRow {
  total: number;
  perRun: number | null;
  inUse: number;
  available: number;
}

export interface ResView {
  rows: Record<ResKey, ResRow>;
  /** False while nothing here can take a run (an offline runner, a switched-off pool): Available reads "Not counted". */
  counted: boolean;
}

export const fmtAmount = (k: ResKey, v: number): string => (k === "cpu" ? RES.CPUS(v) : k === "runs" ? String(v) : RES.GB(v));
/** "4 of 6 CPUs", "8 GB of 12 GB", "2 of 3". */
export const fmtInUse = (k: ResKey, r: ResRow): string => RES.OF(k === "mem" || k === "disk" ? RES.GB(r.inUse) : r.inUse, fmtAmount(k, r.total));
export const usedPct = (r: ResRow): number => (r.total ? Math.min(100, Math.round((r.inUse / r.total) * 100)) : 0);

const tallyRows = (total: RunnerTally, perRun: RunnerAmounts, inUse: RunnerTally, available: RunnerTally): Record<ResKey, ResRow> => ({
  cpu: { total: cpus(total.cpu_millis), perRun: cpus(perRun.cpu_millis), inUse: cpus(inUse.cpu_millis), available: cpus(available.cpu_millis) },
  mem: { total: gb(total.memory_mib), perRun: gb(perRun.memory_mib), inUse: gb(inUse.memory_mib), available: gb(available.memory_mib) },
  disk: { total: gb(total.disk_mib), perRun: gb(perRun.disk_mib), inUse: gb(inUse.disk_mib), available: gb(available.disk_mib) },
  runs: { total: total.runs, perRun: null, inUse: inUse.runs, available: available.runs },
});

/** A runner's table, or null while it declares no limits. */
export function runnerRes(limits: RunnerLimits | null, usage: RunnerUsage): ResView | null {
  if (!limits) return null;
  return { rows: tallyRows({ runs: limits.runs_at_once, ...limits.total }, limits.per_run, usage.in_use, usage.available), counted: usage.online };
}

export interface PoolRes extends ResView {
  runners: number;
  offline: number;
  /** The resources whose per-run size this pool's own limits narrow below its runners'. */
  narrowed: ("cpu" | "mem" | "disk")[];
  /**
   * The resources the pool sets an amount for without narrowing its runners' maximum: one equal to it,
   * wider than it (which never takes effect), or set while the pool has no runner. Its default still
   * applies to a run that asks for nothing.
   */
  set: ("cpu" | "mem" | "disk")[];
}

const ZERO: RunnerTally = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
const add = (a: RunnerTally, b: RunnerTally): RunnerTally => ({
  runs: a.runs + b.runs,
  cpu_millis: a.cpu_millis + b.cpu_millis,
  memory_mib: a.memory_mib + b.memory_mib,
  disk_mib: a.disk_mib + b.disk_mib,
});

const CAP_FIELD = { cpu: "cpu_millis", mem: "memory_mib", disk: "disk_mib" } as const;

/** A pool's own smallest per-run maximum for a resource across the run types it takes, in wire units. */
function poolCap(limits: RunnerPoolLimits | undefined, k: "cpu" | "mem" | "disk"): number | undefined {
  const caps = [limits?.background?.[CAP_FIELD[k]]?.cap, limits?.interactive?.[CAP_FIELD[k]]?.cap].filter((c): c is number => c !== undefined);
  return caps.length ? Math.min(...caps) : undefined;
}

/** A pool across its members: the sums, and the biggest run it can take, narrowed by the pool's own limits. */
export function poolRes(pool: RunnerPool, members: RunnerPoolMemberView[]): PoolRes {
  const active = pool.state === "active";
  let total = ZERO;
  let inUse = ZERO;
  let available = ZERO;
  const largest: RunnerAmounts = { cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  for (const m of members) {
    if (!m.limits) continue;
    total = add(total, { runs: m.limits.runs_at_once, ...m.limits.total });
    inUse = add(inUse, m.usage.in_use);
    if (active) available = add(available, m.usage.available);
    for (const f of Object.values(CAP_FIELD)) largest[f] = Math.max(largest[f], m.limits.per_run[f]);
  }
  const narrowed: PoolRes["narrowed"] = [];
  const set: PoolRes["set"] = [];
  for (const k of ["cpu", "mem", "disk"] as const) {
    const cap = poolCap(pool.limits, k);
    if (cap === undefined) continue;
    if (cap < largest[CAP_FIELD[k]]) {
      largest[CAP_FIELD[k]] = cap;
      narrowed.push(k);
    } else set.push(k);
  }
  return {
    rows: tallyRows(total, largest, inUse, available),
    counted: active,
    runners: members.length,
    offline: members.filter((m) => !m.online).length,
    narrowed,
    set,
  };
}

const names = (keys: ("cpu" | "mem" | "disk")[]) => keys.map((k) => RES_NAME[k].toLowerCase()).join(", ");

/** The pool row's line: "Pool limits: memory per run narrowed; 10 runs maximum", or "" when the pool narrows nothing. It is the
 * approved short form: an amount the pool sets without narrowing is in its details, not on the row. */
export function narrowShort(pool: RunnerPool, r: PoolRes): string {
  const parts: string[] = [];
  if (r.narrowed.length) parts.push(POOLS.NARROWED(names(r.narrowed)));
  if (pool.limits?.max_concurrent_runs) parts.push(POOLS.RUNS_MAX(pool.limits.max_concurrent_runs));
  return parts.length ? POOLS.POOL_LIMITS(parts.join("; ")) : "";
}

/** The pool details' sentence about its own limits: it says the pool adds none only when it sets none. */
export function narrowText(pool: RunnerPool, r: PoolRes): string {
  const parts: string[] = [];
  if (r.narrowed.length) parts.push(POOL_DRAWER.NARROWED_BY_POOL(names(r.narrowed)));
  if (r.set.length) parts.push(POOL_DRAWER.SET_BY_POOL(names(r.set)));
  if (pool.limits?.max_concurrent_runs) parts.push(POOL_DRAWER.RUNS_MAX_IN_POOL(pool.limits.max_concurrent_runs));
  return parts.length ? POOL_DRAWER.POOL_LIMITS(parts.join("; ")) : POOL_DRAWER.NO_OWN_LIMITS;
}

/** The run types a pool takes. A pool with no limits takes both. */
export function poolRunTypes(limits: RunnerPoolLimits | undefined): RunnerPoolRunType[] {
  if (!limits || (!limits.background && !limits.interactive)) return ["background", "interactive"];
  return (["background", "interactive"] as const).filter((t) => limits[t]);
}

export function runTypesText(limits: RunnerPoolLimits | undefined): string {
  const t = poolRunTypes(limits);
  return t.length === 2 ? POOLS.RUN_TYPES_BOTH : t[0] === "background" ? POOLS.RUN_TYPES_BACKGROUND : POOLS.RUN_TYPES_INTERACTIVE;
}

/** "Fence, Wall": the barriers a pool takes; every barrier while it names none. */
export function barriersText(limits: RunnerPoolLimits | undefined): string {
  const classes = limits?.barriers?.length ? limits.barriers : (["CC1", "CC2", "CC3"] as const);
  return classes.map((c) => BARRIER_LABEL[c as keyof typeof BARRIER_LABEL] ?? c).join(", ");
}

/** "1 hour", "30 minutes", "2 days": a duration in its largest whole unit. */
export function fmtDur(sec: number): string {
  const unit = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`;
  if (sec >= 86400 && sec % 86400 === 0) return unit(sec / 86400, "day");
  if (sec >= 3600 && sec % 3600 === 0) return unit(sec / 3600, "hour");
  if (sec % 60 === 0) return unit(sec / 60, "minute");
  return unit(sec, "second");
}

/** Each run's share of a total (display units) when n run side by side: at least 1 CPU and 1 GB, disk in whole GB, never past the total. */
export function perRunShare(total: { cpu: number; mem: number; disk: number }, n: number): { cpu: number; mem: number; disk: number } | null {
  if (!(total.cpu > 0 && total.mem > 0 && total.disk > 0 && Number.isInteger(n) && n >= 1)) return null;
  const share = (v: number, whole: boolean) => Math.min(v, Math.max(1, whole ? Math.floor(v / n) : Math.floor((v / n) * 10) / 10));
  return { cpu: share(total.cpu, false), mem: share(total.mem, false), disk: share(total.disk, true) };
}

/**
 * What setup offers for a computer that reports `reported`, as registration proposes it: half of each as
 * the total, Runs at once about half its CPUs (1 to 64), and total ÷ Runs at once per run.
 */
export function proposeLimits(reported: RunnerAmounts): RunnerLimits {
  const total = {
    cpu: Math.max(1, Math.floor(cpus(reported.cpu_millis) / 2)),
    mem: Math.max(1, Math.floor(gb(reported.memory_mib) / 2)),
    disk: Math.max(1, Math.floor(gb(reported.disk_mib) / 2)),
  };
  const runs = Math.min(64, Math.max(1, Math.floor(cpus(reported.cpu_millis) / 2)));
  const per = perRunShare(total, runs) ?? total;
  return { runs_at_once: runs, total: toAmounts(total), per_run: toAmounts(per) };
}

export const toAmounts = (v: { cpu: number; mem: number; disk: number }): RunnerAmounts => ({
  cpu_millis: Math.round(v.cpu * 1000),
  memory_mib: Math.round(v.mem * 1024),
  disk_mib: Math.round(v.disk * 1024),
});

/**
 * A pool's members from the runner reads an administrator already has: every runner names the pools it
 * is in, so the Pools tab asks nothing more per pool. A revoked runner is in no pool.
 */
export function membersOf(poolId: string, runners: RunnerView[], remote: RemoteRunnerView[]): RunnerPoolMemberView[] {
  const inPool = (pools: { id: string }[]) => pools.some((p) => p.id === poolId);
  return [
    ...runners
      .filter((r) => r.state !== "revoked" && inPool(r.pools))
      .map((r): RunnerPoolMemberView => ({ kind: "self_hosted", id: r.id, name: r.name, online: r.online, runner: r, limits: r.limits, usage: r.usage })),
    ...remote
      .filter((r) => inPool(r.pools))
      .map((r): RunnerPoolMemberView => ({ kind: "remote_hosted", id: r.id, name: r.name, online: r.online, configured: r.configured, limits: r.limits, usage: r.usage })),
  ];
}
