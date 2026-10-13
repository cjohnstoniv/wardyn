/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool details' "Per-run limits" table (mock M-POOLS v6 limitsTable): one row per run type the pool takes.
// A resource reads "default X (max Y)" only where the pool's cap is below its runners' biggest per-run; a cap
// that is absent or no tighter than its runners' never takes effect, so it reads "Up to <what its runners
// allow>, from its runners". Below 560px each row stacks with its own labels, so no column hides behind a
// sideways scroll.
import type { RunnerPool, RunnerPoolAmount, RunnerPoolDuration, RunnerPoolLimits, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import { POOL_DRAWER, RES } from "../../../wardyn/copy/runner-pools";
import { cpus, fmtAmount, fmtDur, gb, poolRunTypes, type ResKey } from "../resources";
import { runnersBiggest } from "./pool-form-model";

type Res = Exclude<ResKey, "runs">;
const FIELD = { cpu: "cpu_millis", mem: "memory_mib", disk: "disk_mib" } as const;
const COLS: { key: Res; label: string }[] = [
  { key: "cpu", label: RES.CPU },
  { key: "mem", label: RES.MEMORY },
  { key: "disk", label: RES.DISK },
];

const shown = (k: Res, wire: number) => fmtAmount(k, k === "cpu" ? cpus(wire) : gb(wire));

/** A duration limit as the sheet words it; an absent one is set by the runners. */
export function durLimit(d: RunnerPoolDuration | undefined, never: boolean): string {
  if (!d) return "—";
  if (d.unlimited) return d.default_sec ? (never ? POOL_DRAWER.DUR_NEVER(fmtDur(d.default_sec)) : POOL_DRAWER.DUR_NO_MAX(fmtDur(d.default_sec))) : POOL_DRAWER.NO_MAXIMUM;
  return POOL_DRAWER.DUR(fmtDur(d.default_sec), fmtDur(d.max_sec ?? 0));
}

function resCell(k: Res, a: RunnerPoolAmount | undefined, biggest: number): string {
  if (a && a.cap < biggest) return POOL_DRAWER.DEFAULT_MAX(shown(k, a.default), shown(k, a.cap));
  return POOL_DRAWER.UP_TO_FROM_RUNNERS(shown(k, biggest));
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
                    {biggest.count ? resCell(c.key, x?.[FIELD[c.key]], biggest[FIELD[c.key]]) : "—"}
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
