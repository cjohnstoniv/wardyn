/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the chosen pool can take and what a run in it gets, from the server's answer for that pool and
// run type. A fact the server did not send leaves its cell out: nothing here is estimated.

import type { ReactNode } from "react";
import type { PersonAllowance, PoolSlots, RunnerPoolCatalogueEntry, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import { cn } from "../../../../ui/utils";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { limitCells, limitsFor, limitsNote, runTypesText } from "./pool-model";

// Make's .pool-summary (App.tsx:1710-1808; snapshot index.css:176-192): a 2 x 2 grid of metrics, then the
// per-run limits four across, each cell a small upper-case label over its value.
const LABEL = "text-[9px] font-semibold tracking-[0.08em] text-muted-foreground uppercase";

function Metric({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1 border-b border-border px-4 py-3.5 min-[521px]:odd:border-r">
      {children}
    </div>
  );
}
const Label = ({ children }: { children: ReactNode }) => <span className={LABEL}>{children}</span>;
const Value = ({ children }: { children: ReactNode }) => <strong className="text-xs leading-[1.4] font-semibold [overflow-wrap:anywhere]">{children}</strong>;
const Sub = ({ children }: { children: ReactNode }) => <small className="text-[10px] leading-[1.4] text-muted-foreground">{children}</small>;

/** A count against a maximum, as text and a thin bar; with no maximum, text only (and no meter role). */
function Usage({ label, ariaLabel, used, max, valueText, personal }: { label: string; ariaLabel: string; used: number; max?: number; valueText: string; personal?: boolean }) {
  return (
    <>
      <Label>{label}</Label>
      <Value>{valueText}</Value>
      {max !== undefined && (
        <span
          role="meter"
          aria-label={ariaLabel}
          aria-valuemin={0}
          aria-valuemax={max}
          aria-valuenow={Math.min(Math.max(used, 0), max)}
          aria-valuetext={valueText}
          className="mt-1 mb-0.5 block h-[5px] w-full overflow-hidden rounded-full bg-border"
        >
          <span className={cn("block h-full rounded-full", personal ? "bg-info" : "bg-primary")} style={{ width: `${max > 0 ? Math.min(100, (used / max) * 100) : 0}%` }} />
        </span>
      )}
    </>
  );
}

function Capacity({ slots }: { slots: PoolSlots }) {
  return (
    <Metric>
      {slots.max === undefined ? (
        <>
          <Usage label={NR_RUN.CAPACITY_LABEL} ariaLabel={NR_RUN.CAPACITY_ARIA} used={slots.used} valueText={NR_RUN.RUNNING_ONLY(slots.used)} />
          <Sub>{NR_RUN.POOL_LIMIT_FROM_RUNNERS}</Sub>
        </>
      ) : (
        <>
          <Usage label={NR_RUN.CAPACITY_LABEL} ariaLabel={NR_RUN.CAPACITY_ARIA} used={slots.used} max={slots.max} valueText={NR_RUN.USED_OF_MAX(slots.used, slots.max)} />
          <Sub>{NR_RUN.MAX_RUNS(slots.max)}</Sub>
        </>
      )}
    </Metric>
  );
}

function Allowance({ allowance }: { allowance: PersonAllowance }) {
  const includes = allowance.active === 1 ? NR_RUN.ALLOWANCE_INCLUDES_ONE : NR_RUN.ALLOWANCE_INCLUDES(allowance.active);
  return (
    <Metric>
      {allowance.max === undefined ? (
        <>
          <Usage label={NR_RUN.ALLOWANCE_LABEL} ariaLabel={NR_RUN.ALLOWANCE_ARIA} used={allowance.active} valueText={NR_RUN.RUNNING_ONLY(allowance.active)} personal />
          <Sub>{NR_RUN.NO_PERSONAL_LIMIT}</Sub>
        </>
      ) : (
        <>
          <Usage label={NR_RUN.ALLOWANCE_LABEL} ariaLabel={NR_RUN.ALLOWANCE_ARIA} used={allowance.active} max={allowance.max} valueText={NR_RUN.ALLOWANCE_USED(allowance.active, allowance.max)} personal />
          <Sub>{includes}</Sub>
        </>
      )}
    </Metric>
  );
}

export function PoolSummary({ pool, runType, allowance }: { pool: RunnerPoolCatalogueEntry; runType: RunnerPoolRunType | undefined; allowance: PersonAllowance | undefined }) {
  const { slots } = pool;
  const limits = limitsFor(pool, runType);
  const cells = limits ? limitCells(limits, allowance) : [];
  return (
    <section aria-label={pool.name} className="mt-4 overflow-hidden rounded-[10px] border border-border-strong bg-surface-2">
      <div className="grid grid-cols-1 min-[521px]:grid-cols-2">
        {slots && <Capacity slots={slots} />}
        {allowance && <Allowance allowance={allowance} />}
        {slots && (
          <Metric>
            <Label>{NR_RUN.WAITING_LABEL}</Label>
            <Value>{slots.waiting > 0 ? NR_RUN.IN_QUEUE(slots.waiting) : NR_RUN.NO_RUNS_WAITING}</Value>
            <Sub>{slots.verdict === "waits" ? NR_RUN.JOIN_QUEUE : NR_RUN.START_WHEN_LAUNCHED}</Sub>
          </Metric>
        )}
        <Metric>
          <Label>{NR_RUN.RUN_TYPES_LABEL}</Label>
          <Value>{runTypesText(pool)}</Value>
          {pool.run_types_source && <Sub>{pool.run_types_source === "pool" ? NR_RUN.SET_BY_POOL : NR_RUN.NO_RESTRICTION}</Sub>}
        </Metric>
      </div>
      {cells.length > 0 && (
        <div>
          <div className="flex items-baseline justify-between gap-3 border-b border-border px-4 py-[13px]">
            <span className={LABEL}>{NR_RUN.LIMITS_LABEL}</span>
            <Sub>{limitsNote(allowance)}</Sub>
          </div>
          <dl className="grid grid-cols-2 min-[521px]:grid-cols-4">
            {cells.map((c) => (
              <div
                key={c.key}
                data-limit={c.key}
                className="flex min-w-0 flex-col gap-1 border-border px-3.5 py-[13px] max-[520px]:nth-[-n+2]:border-b max-[520px]:odd:border-r min-[521px]:border-r min-[521px]:last:border-r-0"
              >
                <dt className={LABEL}>{c.label}</dt>
                <dd className="flex flex-col gap-1">
                  <strong className="font-mono text-[11px] leading-[1.4] font-medium [overflow-wrap:anywhere]">{c.value}</strong>
                  <Sub>{c.note}</Sub>
                  {c.source && <Sub>{c.source}</Sub>}
                </dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </section>
  );
}
