/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Machine card (board KM-3): the kept machine a run keeps or resumed, from GET /runs/{id}'s `machine`. Only
// what that read carries is drawn: the name, the Mac, and what the stop did to the machine. A run that keeps none draws
// nothing. The card sits in the Identity tile: a tile of its own needs an id in the server's closed layout set.

import { HardDrive } from "lucide-react";
import { Link } from "react-router-dom";
import type { RunDetail } from "../../../../lib/types";
import { isTerminalRunState } from "../../../../lib/types/runs";
import { KEEP_LIST, KEEP_RUN as T } from "../../../wardyn/copy/keep-machine";
import { Chip } from "../../../wardyn/primitives";

type Facts = Pick<RunDetail, "machine" | "state" | "failure_hint">;

/** The runner's code words for a run whose machine did not start; the server's failure line carries them. */
const FAILED_START: ReadonlyArray<readonly [string, string | ((mac: string) => string)]> = [
  ["kept_machine_tools_update_failed", T.TOOLS_FAILED],
  ["kept_machine_locked", T.LOCKED],
];

export interface MachineCardView {
  chip: { label: string; tone: "success" | "info" | "warning" | "neutral" };
  /** The line under the rows, bold when it is the card's news (the run ended, the machine is kept). */
  headline?: string;
  /** Smaller lines, in order. */
  notes: string[];
  name?: string;
  mac?: string;
}

/** What the card says for this run's machine, or undefined when the run keeps none. */
export function machineCardOf(run: Facts): MachineCardView | undefined {
  const m = run.machine;
  if (!m) return undefined;
  const mac = m.runner_name || "";
  const where = mac || "its Mac";
  const common = { name: m.name, mac: mac || undefined };
  const live = !isTerminalRunState(run.state);
  if (m.deleted) return { ...common, chip: { label: T.CHIP_DELETED, tone: "neutral" }, notes: [T.DELETED] };
  if (m.state === "deleting") return { ...common, chip: { label: T.CHIP_DELETING, tone: "warning" }, notes: [KEEP_LIST.DELETE_UNCONFIRMED] };
  const failed = run.state === "FAILED" ? FAILED_START.find(([code]) => run.failure_hint?.includes(code)) : undefined;
  if (failed) return { ...common, chip: { label: T.CHIP_KEPT, tone: "warning" }, notes: [typeof failed[1] === "function" ? failed[1](where) : failed[1]] };
  if (m.state === "kept") {
    // Kept while the run is still going: its sandbox stopped at its end time and the machine was released (K-38).
    const scrub = m.scrubbed ? T.SCRUBBED : T.NOT_SCRUBBED;
    return {
      ...common,
      chip: { label: T.CHIP_KEPT, tone: "success" },
      headline: live ? T.HELD(where, m.name ?? "") : T.KEPT(where),
      notes: [scrub],
    };
  }
  // The run is over but the Mac has not reported the stop yet: the machine is still held (K-44).
  if (!live) return { ...common, chip: { label: T.CHIP_KEEPS, tone: "info" }, headline: T.WHILE_RUNNING, notes: [T.STOPPING] };
  const how = m.mode === "new" ? T.NEW(where) : m.mode === "fresh" ? T.FRESH(where) : undefined;
  return { ...common, chip: { label: T.CHIP_KEEPS, tone: "info" }, headline: T.WHILE_RUNNING, notes: how ? [how] : [] };
}

export function MachineCard({ run }: { run: Facts }) {
  const v = machineCardOf(run);
  if (!v) return null;
  return (
    <section aria-labelledby="run-machine-title" data-testid="run-machine-card" className="mb-3 rounded-md border border-border p-3">
      <div className="flex items-center justify-between gap-2">
        <h3 id="run-machine-title" className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          <HardDrive aria-hidden="true" className="size-3.5" />
          {T.TITLE}
        </h3>
        <Chip tone={v.chip.tone}>{v.chip.label}</Chip>
      </div>
      {v.headline && <p className="mt-2 text-xs font-medium text-foreground">{v.headline}</p>}
      {(v.name || v.mac) && (
        <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          {v.name && (
            <>
              <dt className="text-muted-foreground">{T.ROW_NAME}</dt>
              <dd className="min-w-0 break-words text-foreground">{v.name}</dd>
            </>
          )}
          {v.mac && (
            <>
              <dt className="text-muted-foreground">{T.ROW_MAC}</dt>
              <dd className="min-w-0 break-words text-foreground">{v.mac}</dd>
            </>
          )}
        </dl>
      )}
      {v.notes.map((n) => (
        <p key={n} className="mt-2 text-meta leading-relaxed text-muted-foreground">{n}</p>
      ))}
      <Link to="/account#kept-machines" className="mt-2 inline-block text-xs text-primary hover:underline">{T.LINK}</Link>
    </section>
  );
}
