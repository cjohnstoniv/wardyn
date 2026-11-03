/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Machine tile (board KM-3): the kept machine a run keeps or resumed, from GET /runs/{id}'s `machine`. A run
// that keeps none draws nothing. That read is the machine's row as it is now, shared by every run that used the machine,
// and names the run that holds it (or held it last): only on that run's page does the tile say what a stop did to the
// machine. Every other run's page says the machine as it is now.

import { Link } from "react-router-dom";
import { shortDate } from "../../../../lib/format";
import type { RunDetail } from "../../../../lib/types";
import { isTerminalRunState } from "../../../../lib/types/runs";
import { gbOf, KEEP_LIST, KEEP_RUN as T } from "../../../wardyn/copy/keep-machine";
import { Chip, WidgetCard } from "../../../wardyn/primitives";
import { HardDrive } from "lucide-react";

type Facts = Pick<RunDetail, "id" | "machine" | "state" | "failure_hint">;

/**
 * The start of the two sentences the Mac's runner fails a run with when its machine did not start (keptSaidLocked and
 * keptSaidTools, internal/runner/docker/macvm_kept.go); the run's failure line carries the sentence whole. wire-parity.constants.test.ts pins both.
 */
export const KEPT_START_FAILURES = ["This machine is already running on", "This machine's tools couldn't be updated"] as const;

export interface MachineCardView {
  chip: { label: string; tone: "success" | "info" | "warning" | "neutral" };
  /** The line above the rows, bold when it is the card's news (the machine is kept). */
  headline?: string;
  /** Label and value of each row, in order. */
  rows: Array<[label: string, value: string]>;
  /** Smaller lines under the rows, in order. */
  notes: string[];
}

/** What the card says for this run's machine, or undefined when the run keeps none. */
export function machineCardOf(run: Facts): MachineCardView | undefined {
  const m = run.machine;
  if (!m) return undefined;
  const mac = m.runner_name || "";
  const where = mac || KEEP_LIST.THIS_MAC;
  const live = !isTerminalRunState(run.state);
  // The run that holds the machine, or held it last. A server that names none leaves only a live run sure of it.
  const mine = m.holder_run_id ? m.holder_run_id === run.id : live;
  const rows = (...extra: Array<[string, string] | false>): Array<[string, string]> => [
    ...(m.name ? [[T.ROW_NAME, m.name] as [string, string]] : []),
    ...extra.filter((r): r is [string, string] => !!r),
  ];
  const kept = (): Array<[string, string]> =>
    rows(m.disk_bytes ? [T.ROW_WRITTEN, `${gbOf(m.disk_bytes)} GB`] : false, m.expires_at ? [T.ROW_EXPIRES, shortDate(m.expires_at)] : false);

  if (m.deleted) return { chip: { label: T.CHIP_DELETED, tone: "neutral" }, rows: rows(), notes: [T.DELETED] };
  if (m.state === "deleting") return { chip: { label: T.CHIP_DELETING, tone: "warning" }, rows: rows(), notes: [KEEP_LIST.DELETE_UNCONFIRMED] };
  const failed = run.state === "FAILED" && run.failure_hint && KEPT_START_FAILURES.some((s) => run.failure_hint?.includes(s));
  if (failed && run.failure_hint) {
    const chip = m.state === "kept" ? { label: T.CHIP_KEPT, tone: "warning" as const } : { label: KEEP_LIST.CHIP_IN_USE, tone: "info" as const };
    return { chip, rows: rows(), notes: [run.failure_hint] };
  }
  if (m.state === "kept") {
    if (!mine) {
      const unscrubbed = !m.scrubbed;
      return {
        chip: unscrubbed ? { label: KEEP_LIST.CHIP_NOT_CLEANED, tone: "warning" } : { label: T.CHIP_KEPT, tone: "success" },
        headline: T.KEPT(where),
        rows: kept(),
        notes: [...(unscrubbed ? [T.NEEDS_CLEANUP] : []), T.AS_NOW],
      };
    }
    const scrub = m.scrubbed ? T.SCRUBBED : T.NOT_SCRUBBED;
    return { chip: { label: T.CHIP_KEPT, tone: "success" }, headline: live ? T.HELD(where, m.name ?? "") : T.KEPT(where), rows: kept(), notes: [scrub] };
  }
  // In use, or being made.
  if (!mine) return { chip: { label: KEEP_LIST.CHIP_IN_USE, tone: "info" }, rows: rows(), notes: [T.BUSY] };
  // This run holds it and has ended: the Mac has not reported the stop yet (K-44).
  if (!live) return { chip: { label: T.CHIP_KEEPS, tone: "info" }, headline: T.WHILE_RUNNING, rows: rows([T.ROW_MAC, where]), notes: [T.STOPPING] };
  const how = m.mode === "new" ? T.NEW(where) : m.mode === "fresh" ? T.FRESH(where) : m.mode === "resumed" ? T.RESUMED : undefined;
  return {
    chip: { label: T.CHIP_KEEPS, tone: "info" },
    headline: T.WHILE_RUNNING,
    rows: rows(mac ? [T.ROW_MAC, mac] : false, m.base ? [T.ROW_BASE, m.base] : false),
    notes: how ? [how] : [],
  };
}

/** The Machine tile: nothing for a run that keeps no machine. */
export function MachineWidget({ run }: { run: Facts }) {
  const v = machineCardOf(run);
  if (!v) return null;
  return (
    <WidgetCard title={T.TITLE} Icon={HardDrive} right={<Chip tone={v.chip.tone}>{v.chip.label}</Chip>} data-testid="run-machine-card">
      {v.headline && <p className="text-xs font-medium text-foreground">{v.headline}</p>}
      {v.rows.length > 0 && (
        <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          {v.rows.map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-muted-foreground">{label}</dt>
              <dd className="min-w-0 break-words text-foreground">{value}</dd>
            </div>
          ))}
        </dl>
      )}
      {v.notes.map((n) => (
        <p key={n} className="mt-2 text-meta leading-relaxed text-muted-foreground">{n}</p>
      ))}
      <Link to="/account#kept-machines" className="mt-2 inline-block text-xs text-primary hover:underline">{T.LINK}</Link>
    </WidgetCard>
  );
}
