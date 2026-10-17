/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form's pure model (mock M-POOLS v6 app.js openForm / validate / toLimits): the form holds display
// units (CPUs, GB, a number and a unit for each duration), the wire holds milli-CPU, MiB and seconds. Errors are
// keyed as the mock keys them (name, types, barriers, maxruns, "bg.cpu", "bg.life", "it.idle", ...), so the form
// puts each sentence under its field. The server's own ceilings are not checked here: a pool limit wider than
// its runners is accepted and never takes effect (the pool details show it as "Up to ... from its runners").
import type { RunnerPoolAmount, RunnerPoolDuration, RunnerPoolLimits, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import type { RunnerAmounts } from "../../../../lib/types/runners";
import { BARRIER_LABEL } from "../../../../lib/runner-pool-refusals";
import { POOL_DRAWER, POOL_FORM, RES } from "../../../wardyn/copy/runner-pools";
import { fmtDur } from "../resources";

export type Unit = "minutes" | "hours" | "days";
/** The unit words the form's selects show (the mock's option text). */
export const UNIT_LABEL: Record<Unit, string> = { minutes: "minutes", hours: "hours", days: "days" };
export const UNITS: Unit[] = ["minutes", "hours", "days"];
const SECS: Record<Unit, number> = { minutes: 60, hours: 3600, days: 86400 };

/** One run type's fields; every number is the text of its input. */
export interface RtValues {
  cpuD: string;
  cpuC: string;
  memD: string;
  memC: string;
  diskD: string;
  diskC: string;
  lifeDn: string;
  lifeDu: Unit;
  lifeMn: string;
  lifeMu: Unit;
  lifeUnl: boolean;
  /** Background only: "Runs default to No end" (a default of 0 under No maximum). */
  lifeNoEnd: boolean;
  idleDn: string;
  idleDu: Unit;
  idleMn: string;
  idleMu: Unit;
  idleUnl: boolean;
}

export interface FormValues {
  name: string;
  bgOn: boolean;
  itOn: boolean;
  cc1: boolean;
  cc2: boolean;
  cc3: boolean;
  maxruns: string;
  bg: RtValues;
  it: RtValues;
}

export type RtKey = "bg" | "it";
export type FormErrors = Record<string, string>;

/** The mock's blankRt: what a NEW pool's run type starts with. */
export const blankRt = (interactive: boolean): RtValues => ({
  cpuD: "",
  cpuC: "",
  memD: "",
  memC: "",
  diskD: "",
  diskC: "",
  lifeDn: interactive ? "8" : "1",
  lifeDu: "hours",
  lifeMn: "24",
  lifeMu: "hours",
  lifeUnl: false,
  lifeNoEnd: false,
  idleDn: "30",
  idleDu: "minutes",
  idleMn: "2",
  idleMu: "hours",
  idleUnl: false,
});

/** An edit of a pool that sets no duration leaves the pair empty: it stays unset unless the person fills it. */
const emptyRt = (): RtValues => ({ ...blankRt(false), lifeDn: "", lifeMn: "", idleDn: "", idleMn: "" });

const toDu = (sec: number): [string, Unit] =>
  sec % 86400 === 0 ? [String(sec / 86400), "days"] : sec % 3600 === 0 ? [String(sec / 3600), "hours"] : [String(Math.round(sec / 60)), "minutes"];

const amountText = (a: RunnerPoolAmount | undefined, per: number) => ({ d: a ? String(a.default / per) : "", c: a ? String(a.cap / per) : "" });

type RtLimits = {
  cpu_millis?: RunnerPoolAmount;
  memory_mib?: RunnerPoolAmount;
  disk_mib?: RunnerPoolAmount;
  lifetime?: RunnerPoolDuration;
  idle?: RunnerPoolDuration;
};

function rtFrom(x: RtLimits): RtValues {
  const o = emptyRt();
  const cpu = amountText(x.cpu_millis, 1000);
  const mem = amountText(x.memory_mib, 1024);
  const disk = amountText(x.disk_mib, 1024);
  Object.assign(o, { cpuD: cpu.d, cpuC: cpu.c, memD: mem.d, memC: mem.c, diskD: disk.d, diskC: disk.c });
  const dur = (d: RunnerPoolDuration | undefined, pre: "life" | "idle") => {
    if (!d) return;
    const [dn, du] = toDu(d.default_sec || 3600);
    const [mn, mu] = toDu(d.max_sec || 86400);
    Object.assign(o, { [`${pre}Dn`]: dn, [`${pre}Du`]: du, [`${pre}Mn`]: mn, [`${pre}Mu`]: mu, [`${pre}Unl`]: !!d.unlimited });
  };
  dur(x.lifetime, "life");
  dur(x.idle, "idle");
  o.lifeNoEnd = !!x.lifetime?.unlimited && !x.lifetime.default_sec;
  return o;
}

/** The form's starting values: a pool's own limits, or the new-pool defaults. */
export function fromPool(pool?: { name: string; limits?: RunnerPoolLimits }): FormValues {
  if (!pool) {
    return { name: "", bgOn: true, itOn: true, cc1: true, cc2: true, cc3: false, maxruns: "", bg: blankRt(false), it: blankRt(true) };
  }
  const l = pool.limits;
  // A pool that names no run type takes both; one that names no barrier takes every barrier.
  const both = !l || (!l.background && !l.interactive);
  const barriers = l?.barriers?.length ? l.barriers : (["CC1", "CC2", "CC3"] as const);
  return {
    name: pool.name,
    bgOn: both || !!l?.background,
    itOn: both || !!l?.interactive,
    cc1: barriers.includes("CC1"),
    cc2: barriers.includes("CC2"),
    cc3: barriers.includes("CC3"),
    maxruns: l?.max_concurrent_runs ? String(l.max_concurrent_runs) : "",
    bg: l?.background ? rtFrom(l.background) : both ? emptyRt() : blankRt(false),
    it: l?.interactive ? rtFrom(l.interactive) : both ? emptyRt() : blankRt(true),
  };
}

const blank = (x: string) => x.trim() === "";
const num = (x: string) => (blank(x) ? NaN : Number(x));
const RT_TITLE: Record<RtKey, string> = { bg: POOL_DRAWER.BACKGROUND, it: POOL_DRAWER.INTERACTIVE };
const RES_WORD = { cpu: RES.CPU, mem: RES.MEMORY.toLowerCase(), disk: RES.DISK.toLowerCase() } as const;

/** A duration pair left wholly empty is unset; the pool's runners set it. */
const durEmpty = (x: RtValues, pre: "life" | "idle", noEnd: boolean) => blank(x[`${pre}Dn`]) && blank(x[`${pre}Mn`]) && !x[`${pre}Unl`] && !noEnd;

/** `loaded` is the pool's limits as the form read them: a duration field left as loaded counts as it was read. */
export function validate(v: FormValues, loaded?: RunnerPoolLimits): FormErrors {
  const e: FormErrors = {};
  if (!v.name || v.name !== v.name.trim()) e.name = POOL_FORM.ERR_NAME;
  else if ([...v.name].length > 64) e.name = POOL_FORM.ERR_NAME_LONG;
  if (!v.bgOn && !v.itOn) e.types = POOL_FORM.ERR_TYPES;
  if (!v.cc1 && !v.cc2 && !v.cc3) e.barriers = POOL_FORM.ERR_BARRIERS;
  if (!blank(v.maxruns) && !(Number.isInteger(num(v.maxruns)) && num(v.maxruns) >= 1)) e.maxruns = POOL_FORM.ERR_MAX_RUNS;
  for (const k of ["bg", "it"] as const) {
    if (!v[`${k}On`]) continue;
    const x = v[k];
    const from = loaded?.[k === "bg" ? "background" : "interactive"];
    const title = RT_TITLE[k];
    for (const r of ["cpu", "mem", "disk"] as const) {
      const d = x[`${r}D`];
      const c = x[`${r}C`];
      const key = `${k}.${r}`;
      if (blank(d) !== blank(c)) e[key] = POOL_FORM.ERR_BOTH(title, RES_WORD[r]);
      else if (blank(d)) continue;
      else if (!(num(c) > 0)) e[key] = POOL_FORM.ERR_MAX_ABOVE_ZERO(title, RES_WORD[r]);
      else if (!(num(d) > 0)) e[key] = POOL_FORM.ERR_DEFAULT_ABOVE_ZERO(title, RES_WORD[r]);
      else if (r === "disk" && !(Number.isInteger(num(d)) && Number.isInteger(num(c)))) e[key] = POOL_FORM.ERR_DISK_WHOLE(title);
      else if (num(d) > num(c)) e[key] = POOL_FORM.ERR_DEFAULT_OVER_MAX(title, RES_WORD[r], num(d), num(c));
    }
    const dur = (pre: "life" | "idle", what: string, never: string) => {
      const noEnd = k === "bg" && pre === "life" && x.lifeUnl && x.lifeNoEnd;
      if (durEmpty(x, pre, noEnd)) return;
      const { dsec, msec, keepD } = durSecs(x, pre, noEnd, from);
      const unl = x[`${pre}Unl`];
      const key = `${k}.${pre === "life" ? "life" : "idle"}`;
      if (!noEnd && !keepD && !(dsec >= 1)) e[key] = POOL_FORM.ERR_DUR_DEFAULT(title, what);
      // A Maximum error is also filed under `${key}.max` so the form marks that input, not the Default.
      else if (!unl && !(msec >= 1)) e[key] = e[`${key}.max`] = POOL_FORM.ERR_DUR_MAX(title, what, never);
      else if (!unl && dsec > msec) e[key] = e[`${key}.max`] = POOL_FORM.ERR_DUR_ORDER(title, what, fmtDur(Math.round(dsec)), fmtDur(Math.round(msec)));
    };
    dur("life", POOL_FORM.LIFETIME.toLowerCase(), POOL_FORM.NO_MAXIMUM);
    if (k === "it") dur("idle", POOL_FORM.IDLE.toLowerCase(), POOL_FORM.NEVER);
  }
  return e;
}

const amount = (d: string, c: string, per: number): RunnerPoolAmount | undefined =>
  blank(d) && blank(c) ? undefined : { default: Math.round(num(d) * per), cap: Math.round(num(c) * per) };


/**
 * A duration's seconds as a save will send them. The form shows a duration in whole minutes, hours or days
 * and an unset default as 1 hour, so each of the default and the maximum the person did not touch goes back
 * as it was read from `from` (90 s stays 90 s, an idle stop that never stops by default stays default 0, a
 * 10 s maximum is not rounded to 0 minutes) and only a field they changed converts from what they typed.
 */
function durSecs(x: RtValues, pre: "life" | "idle", noEnd: boolean, from?: RtLimits) {
  const src = pre === "life" ? from?.lifetime : from?.idle;
  const o = from && rtFrom(from);
  const same = (a: "Dn" | "Mn", b: "Du" | "Mu") => !!o && x[`${pre}${a}`] === o[`${pre}${a}`] && x[`${pre}${b}`] === o[`${pre}${b}`];
  const unl = x[`${pre}Unl`];
  const keepD = !!src && same("Dn", "Du") && (src.default_sec > 0 || unl) && (pre !== "life" || x.lifeNoEnd === o?.lifeNoEnd);
  const typed = (n: "Dn" | "Mn", u: "Du" | "Mu") => Math.round(num(x[`${pre}${n}`]) * SECS[x[`${pre}${u}`]]);
  const dsec = keepD ? src.default_sec : noEnd ? 0 : typed("Dn", "Du");
  // An untouched maximum goes back exactly, unless the default now outgrows it but fits the maximum as the field shows it (150 s shows as
  // 3 minutes): a default typed to match what is on screen is not refused.
  const keepM = same("Mn", "Mu") && !!src?.max_sec && !(dsec > src.max_sec && typed("Mn", "Mu") >= dsec);
  return {
    keepD,
    dsec,
    msec: (keepM && src?.max_sec) || typed("Mn", "Mu"),
  };
}

function duration(x: RtValues, pre: "life" | "idle", noEnd: boolean, from?: RtLimits): RunnerPoolDuration | undefined {
  if (durEmpty(x, pre, noEnd)) return undefined;
  const { dsec, msec } = durSecs(x, pre, noEnd, from);
  return x[`${pre}Unl`] ? { default_sec: dsec, unlimited: true } : { default_sec: dsec, max_sec: msec };
}

const defined = <T extends object>(o: T): T => Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as T;

function rtLimits(x: RtValues, interactive: boolean, from?: RtLimits): RtLimits {
  const base = defined({
    cpu_millis: amount(x.cpuD, x.cpuC, 1000),
    memory_mib: amount(x.memD, x.memC, 1024),
    disk_mib: amount(x.diskD, x.diskC, 1024),
    lifetime: duration(x, "life", !interactive && x.lifeUnl && x.lifeNoEnd, from),
  });
  return interactive ? defined({ ...base, idle: duration(x, "idle", false, from) }) : base;
}

/**
 * The form's values as the wire's limits (the values must have passed validate). `loaded` is the pool's
 * limits as the form read them: a duration left as loaded is sent as read. Amounts round-trip exactly.
 */
export function toLimits(v: FormValues, loaded?: RunnerPoolLimits): RunnerPoolLimits {
  const limits: RunnerPoolLimits = { barriers: (["CC1", "CC2", "CC3"] as const).filter((_, i) => v[`cc${i + 1}` as "cc1" | "cc2" | "cc3"]) };
  if (!blank(v.maxruns)) limits.max_concurrent_runs = Number(v.maxruns);
  if (v.bgOn) limits.background = rtLimits(v.bg, false, loaded?.background);
  if (v.itOn) limits.interactive = rtLimits(v.it, true, loaded?.interactive);
  return limits;
}

/** The biggest per-run amounts any of the pool's runners allows, wire units; all zero while it has none. */
export function runnersBiggest(members: RunnerPoolMemberView[]): RunnerAmounts & { count: number } {
  const out = { cpu_millis: 0, memory_mib: 0, disk_mib: 0, count: 0 };
  for (const m of members) {
    if (!m.limits) continue;
    out.count++;
    out.cpu_millis = Math.max(out.cpu_millis, m.limits.per_run.cpu_millis);
    out.memory_mib = Math.max(out.memory_mib, m.limits.per_run.memory_mib);
    out.disk_mib = Math.max(out.disk_mib, m.limits.per_run.disk_mib);
  }
  return out;
}

/** The sum of its runners' Runs at once. */
export const runsTotal = (members: RunnerPoolMemberView[]): number => members.reduce((n, m) => n + (m.limits?.runs_at_once ?? 0), 0);

export const BARRIER_FIELDS = [
  { id: "cc1", label: BARRIER_LABEL.CC1 },
  { id: "cc2", label: BARRIER_LABEL.CC2 },
  { id: "cc3", label: BARRIER_LABEL.CC3 },
] as const;
