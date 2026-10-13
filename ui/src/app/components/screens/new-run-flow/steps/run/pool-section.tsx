/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Label } from "../../../../ui/label";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { RUNNER_POOL_REFUSAL } from "../../../../../lib/runner-pool-refusals";
import type { RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import type { RunIntent } from "../../draft/run-intent";
import { poolChoices, selectedPoolId, usablePools } from "./pool-model";
import type { PoolsState } from "./pool-source";
import { PoolNotes } from "./pool-notes";
import { PoolSummary } from "./pool-summary";

export const POOL_ID = "nr-pool";
const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm aria-invalid:border-destructive";

/** The pool field of "Where should it run?": the pools the person may use, each disabled one with its reason, and what the chosen pool offers. */
export function PoolSection({ state, runType, intent, onChoose }: {
  state: PoolsState; runType: RunnerPoolRunType | undefined; intent: RunIntent; onChoose: (id: string) => void;
}) {
  const [flagged, setFlagged] = React.useState(false);
  const list = state.status === "ready" ? state.list : undefined;
  const pools = list ? usablePools(list) : [];
  const value = list ? selectedPoolId(intent, list) : "";
  const chosen = pools.find((p) => p.id === value);
  const missing = flagged && !!list && !value;
  return (
    <div>
      <div className="flex flex-col gap-2">
        <Label htmlFor={POOL_ID}>{NR_RUN.POOL_LABEL}</Label>
        <select
          id={POOL_ID}
          className={SELECT_CLASS}
          value={value}
          disabled={!list}
          aria-busy={state.status === "loading" || undefined}
          aria-describedby={`${POOL_ID}-help`}
          // The shell marks the field invalid before it focuses it when Continue finds no pool.
          onFocus={(e) => e.currentTarget.getAttribute("aria-invalid") === "true" && setFlagged(true)}
          onChange={(e) => onChoose(e.target.value)}
        >
          {!value && <option value="" disabled hidden />}
          {list && poolChoices(pools, runType).map((c) => <option key={c.id} value={c.id} disabled={c.disabled}>{c.label}</option>)}
        </select>
        {missing && <p role="alert" className="text-xs text-danger">{RUNNER_POOL_REFUSAL.REQUIRED()}</p>}
        {state.status === "error" && <p role="alert" className="text-xs text-danger">{state.message}</p>}
        <p id={`${POOL_ID}-help`} className="text-xs leading-snug text-muted-foreground">{NR_RUN.POOL_HELP}</p>
        {list && <PoolNotes pools={pools} chosen={chosen} runType={runType} />}
      </div>
      {chosen && <PoolSummary pool={chosen} runType={runType} allowance={list?.allowance} />}
    </div>
  );
}
