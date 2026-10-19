/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool details' "Per-run limits" table (mock M-POOLS v6 limitsTable): one row per run type the pool takes.
// A resource the pool sets reads "default X (max Y)"; one it leaves to its runners reads "Up to <what its
// runners allow>, from its runners". A maximum wider than its runners never takes effect, so that cell shows
// both: what the pool set, and what bounds the run. Below 560px each row stacks with its own labels, so no
// column hides behind a sideways scroll.
import type * as React from "react";
import type { RunnerPool, RunnerPoolAmount, RunnerPoolDuration, RunnerPoolLimits, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import { POOL_DRAWER, RES } from "../../../wardyn/copy/runner-pools";
import { formatDuration } from "../../../../lib/format";
import { cpus, fmtAmount, gb, poolRunTypes, type ResKey } from "../resources";
import { runnersBiggest } from "./pool-form-model";

type Res = Exclude<ResKey, "runs">;
const FIELD = { cpu: "cpu_millis", mem: "memory_mib", disk: "disk_mib" } as const;
const COLS: { key: Res; label: string }[] = [
  { key: "cpu", label: RES.CPU },
  { key: "mem", label: RES.MEMORY },
  { key: "disk", label: RES.DISK },
];

const dur = (sec: number) => formatDuration(sec * 1000, "words");
const shown = (k: Res, wire: number) => fmtAmount(k, k === "cpu" ? cpus(wire) : gb(wire));

/** A duration limit as the sheet words it; an absent one is set by the runners. */
export function durLimit(d: RunnerPoolDuration | undefined, never: boolean): string {
  if (!d) return "—";
  if (d.unlimited) return d.default_sec ? (never ? POOL_DRAWER.DUR_NEVER(dur(d.default_sec)) : POOL_DRAWER.DUR_NO_MAX(dur(d.default_sec))) : never ? POOL_DRAWER.NEVER : POOL_DRAWER.NO_MAXIMUM;
  return POOL_DRAWER.DUR(dur(d.default_sec), dur(d.max_sec ?? 0));
}

/**
 * One resource of one run type. An amount the pool sets is always shown as set, default and maximum: the
 * default applies to a run that asks for nothing whatever the runners allow. Where the runners allow
 * less than the pool's maximum, what they allow is shown too, because that is what bounds the run.
 */
function resCell(k: Res, a: RunnerPoolAmount | undefined, biggest: number, runners: number): React.ReactNode {
  const fromRunners = runners > 0 ? POOL_DRAWER.UP_TO_FROM_RUNNERS(shown(k, biggest)) : "—";
  if (!a) return fromRunners;
  const own = POOL_DRAWER.DEFAULT_MAX(shown(k, a.default), shown(k, a.cap));
  if (runners === 0 || a.cap <= biggest) return own;
  return (
    <>
      <span className="block">{own}</span>
      <span className="block text-muted-foreground">{fromRunners}</span>
    </>
  );
}

const TD =
  "border-t border-border py-1.5 pr-2 align-top max-sm:block max-sm:border-0 max-sm:py-0.5 max-sm:before:content-[attr(data-l)] max-sm:before:text-xs max-sm:before:text-muted-foreground";

export function PoolLimitsTable({ pool, members }: { pool: RunnerPool; members: RunnerPoolMemberView[] }) {
  const biggest = runnersBiggest(members);
  const limits: RunnerPoolLimits | undefined = pool.limits;
  const types = poolRunTypes(limits);
  return (
    <div className="scroll-thin overflow-x-auto">
      <table className="w-full border-collapse text-body max-sm:block">
        <thead className="max-sm:hidden">
          <tr>
            {[POOL_DRAWER.COL_RUN_TYPE, ...COLS.map((c) => c.label), POOL_DRAWER.COL_LIFETIME, POOL_DRAWER.COL_IDLE].map((h) => (
              <th key={h} className="label-eyebrow py-1 pr-2 text-left">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="max-sm:block">
          {types.map((t) => {
            const x = limits?.[t];
            return (
              <tr key={t} data-run-type={t} className="max-sm:block max-sm:border-t max-sm:border-border max-sm:py-1.5">
                <td className={`${TD} max-sm:before:content-none`}>
                  <strong className="font-semibold">{t === "background" ? POOL_DRAWER.BACKGROUND : POOL_DRAWER.INTERACTIVE}</strong>
                </td>
                {COLS.map((c) => (
                  <td key={c.key} data-l={`${c.label}: `} className={TD}>
                    {resCell(c.key, x?.[FIELD[c.key]], biggest[FIELD[c.key]], biggest.count)}
                  </td>
                ))}
                <td data-l={`${POOL_DRAWER.COL_LIFETIME}: `} className={TD}>
                  {durLimit(x?.lifetime, false)}
                </td>
                <td data-l={`${POOL_DRAWER.COL_IDLE}: `} className={TD}>
                  {t === "interactive" ? durLimit(limits?.interactive?.idle, true) : POOL_DRAWER.NOT_APPLICABLE}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
