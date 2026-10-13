/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the chosen pool can take and what a run in it gets, from the server's answer for that pool and
// run type. A fact the server did not send leaves its cell out: nothing here is estimated.

import type { ReactNode } from "react";
import type { PersonAllowance, PoolSlots, RunnerPoolCatalogueEntry, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { UsageMeter } from "../../../../wardyn/usage-meter";
import { limitCells, limitsFor, limitsNote, runTypesText } from "./pool-model";

function Metric({ label, children }: { label?: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      {label && <span className="text-xs text-muted-foreground">{label}</span>}
      {children}
    </div>
  );
}
const Sub = ({ children }: { children: ReactNode }) => <small className="text-xs text-muted-foreground">{children}</small>;

function Capacity({ slots }: { slots: PoolSlots }) {
  return (
    <Metric>
      {slots.max === undefined ? (
        <>
          <UsageMeter label={NR_RUN.CAPACITY_LABEL} used={slots.used} valueText={NR_RUN.RUNNING_ONLY(slots.used)} />
          <Sub>{NR_RUN.POOL_LIMIT_FROM_RUNNERS}</Sub>
        </>
      ) : (
        <>
          <UsageMeter label={NR_RUN.CAPACITY_LABEL} used={slots.used} max={slots.max} valueText={NR_RUN.USED_OF_MAX(slots.used, slots.max)} />
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
          <UsageMeter label={NR_RUN.ALLOWANCE_LABEL} used={allowance.active} valueText={NR_RUN.RUNNING_ONLY(allowance.active)} />
          <Sub>{NR_RUN.NO_PERSONAL_LIMIT}</Sub>
        </>
      ) : (
        <>
          <UsageMeter label={NR_RUN.ALLOWANCE_LABEL} used={allowance.active} max={allowance.max} valueText={NR_RUN.ALLOWANCE_USED(allowance.active, allowance.max)} />
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
    <section aria-label={pool.name} className="flex flex-col gap-4 rounded-lg border border-border p-4">
      <div className="grid gap-4 sm:grid-cols-2">
        {slots && <Capacity slots={slots} />}
        {allowance && <Allowance allowance={allowance} />}
        {slots && (
          <Metric label={NR_RUN.WAITING_LABEL}>
            <strong className="text-sm font-medium">{slots.waiting > 0 ? NR_RUN.IN_QUEUE(slots.waiting) : NR_RUN.NO_RUNS_WAITING}</strong>
            <Sub>{slots.verdict === "waits" ? NR_RUN.JOIN_QUEUE : NR_RUN.START_WHEN_LAUNCHED}</Sub>
          </Metric>
        )}
        <Metric label={NR_RUN.RUN_TYPES_LABEL}>
          <strong className="text-sm font-medium">{runTypesText(pool)}</strong>
          {pool.run_types_source && <Sub>{pool.run_types_source === "pool" ? NR_RUN.SET_BY_POOL : NR_RUN.NO_RESTRICTION}</Sub>}
        </Metric>
      </div>
      {cells.length > 0 && (
        <div className="flex flex-col gap-3 border-t border-border pt-4">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <span className="text-sm font-medium">{NR_RUN.LIMITS_LABEL}</span>
            <Sub>{limitsNote(allowance)}</Sub>
          </div>
          <dl className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {cells.map((c) => (
              <div key={c.key} data-limit={c.key} className="flex min-w-0 flex-col gap-1">
                <dt className="text-xs text-muted-foreground">{c.label}</dt>
                <dd className="flex flex-col gap-1">
                  <strong className="text-sm font-medium">{c.value}</strong>
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
