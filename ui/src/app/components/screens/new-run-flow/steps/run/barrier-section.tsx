/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { BarrierCompareTable } from "../../../../wardyn/barrier-compare-table";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import type { RunnerPoolCatalogueEntry } from "../../../../../lib/types/runner-pools";
import type { ConfinementClass } from "../../../../../lib/types/runs";
import { poolScope } from "../../lenses/run-lens";
import { barrierRows } from "./pool-model";

/**
 * The three barriers of the chosen pool. The server's pick is selected until the person chooses another; only
 * a choice is written to the draft. "Where it runs" shows for a User pool, whose runners are the person's own.
 */
export function BarrierSection({ pool, chosen, onChoose }: {
  pool: RunnerPoolCatalogueEntry | undefined; chosen: ConfinementClass | undefined; onChoose: (cc: ConfinementClass) => void;
}) {
  const { options, pick } = barrierRows(pool);
  const selected = chosen ?? pick?.cc;
  return (
    // Make's .barrier-field (App.tsx:1798-1912; snapshot index.css:193-194, 239).
    <div className="mt-[18px]">
      <span className="mb-[9px] block text-xs font-semibold">{NR_RUN.BARRIER_LABEL}</span>
      <BarrierCompareTable
        label={NR_RUN.BARRIER_LABEL}
        options={options}
        value={selected}
        onChange={onChoose}
        defaultBadge={pick && { cc: pick.cc, text: pick.badge }}
        badges={{ CC3: NR_RUN.BADGE_EXPERIMENTAL }}
        showWhere={!!pool && poolScope(pool.scope) === "user"}
      />
      <p className="mx-0.5 mt-3 text-[11px] leading-normal text-muted-foreground">{NR_RUN.POLICY_NOTE}</p>
    </div>
  );
}
