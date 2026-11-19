/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Data out card in the Wardyn boundary's hard limits: one row, the rule for content leaving a run, under a
// header with the value at a glance. The boundary's value reaches every run, break-glass included, and a
// profile below may only hold in more. "Lock for profiles below" adds data_out to the boundary's locked_knobs.
import { BOUNDARY_DESKTOP as B } from "../../../lib/boundary-copy";
import { DATA_OUT as C, type DataOutLevel } from "../../wardyn/copy/data-out";

const LOCK = "data_out";

export function DataOutCard({
  value,
  locks,
  disabled,
  onChange,
  onLocksChange,
}: {
  value: DataOutLevel | undefined;
  locks: readonly string[];
  disabled: boolean;
  onChange: (v: DataOutLevel) => void;
  onLocksChange: (locks: string[]) => void;
}) {
  const shown = value ?? "allowed";
  const locked = locks.includes(LOCK);
  return (
    <section aria-labelledby="boundary-data-out-title" data-testid="boundary-data-out" className="rounded-lg border border-border">
      <header className="flex flex-col gap-1 border-b border-border px-4 py-3">
        <div className="flex flex-wrap items-baseline gap-2">
          <h3 id="boundary-data-out-title" className="text-sm font-medium text-foreground">
            {C.LABEL}
          </h3>
          <span className="text-xs text-muted-foreground">{C.BOUNDARY_COUNT}</span>
        </div>
        <p className="text-xs text-muted-foreground">{C.BOUNDARY_APPLIES}</p>
        <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs" data-testid="boundary-data-out-now">
          <span className="font-medium text-foreground">{B.NOW}</span>
          <span className="text-muted-foreground">{C.BOUNDARY_ROW}</span>
          <span className="font-medium text-foreground">{C.label(shown)}</span>
        </p>
      </header>
      <div className="px-4 py-3">
        <fieldset disabled={disabled} className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <legend className="sr-only">{C.BOUNDARY_ROW}</legend>
          <span aria-hidden="true" className="min-w-[13rem] text-sm font-medium text-foreground">
            {C.BOUNDARY_ROW}
          </span>
          <span className="inline-flex overflow-hidden rounded-md border border-border text-xs">
            {C.LEVELS.map((o) => (
              <label key={o.value} className="cursor-pointer">
                <input
                  type="radio"
                  className="peer sr-only"
                  name="boundary-data-out"
                  value={o.value}
                  checked={shown === o.value}
                  onChange={() => onChange(o.value)}
                />
                <span className="block px-3 py-1.5 text-muted-foreground peer-checked:bg-primary peer-checked:text-primary-foreground peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-disabled:cursor-not-allowed peer-disabled:opacity-60">
                  {o.label}
                </span>
              </label>
            ))}
          </span>
          <span className="text-xs text-muted-foreground">{C.BOUNDARY_BELOW}</span>
          <label className="flex basis-full items-center gap-2 text-xs text-foreground">
            <input
              type="checkbox"
              checked={locked}
              onChange={(e) => onLocksChange(e.target.checked ? [...locks.filter((l) => l !== LOCK), LOCK] : locks.filter((l) => l !== LOCK))}
            />
            {B.LOCK_ROW}
          </label>
        </fieldset>
      </div>
      <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">{C.BOUNDARY_ENFORCED}</p>
    </section>
  );
}
