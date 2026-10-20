/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one resource table (mock M-POOLS v6): Total, Per run (a pool: Biggest run it can take), In use with
// its bar, and Available, for CPU, memory, disk and runs. A runner that is offline, and a pool that is
// switched off, show "Not counted" for Available: their capacity is never offered. Below 640px each
// resource stacks with its own labels, so no column is cut off behind a sideways scroll.
import * as React from "react";
import { relativeTime } from "../../../lib/format";
import type { RunnerKind } from "../../../lib/types/runner-pools";
import type { RunnerFailure } from "../../../lib/types/runners";
import { RES, RUNNER_KIND } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Notice } from "../../wardyn/notice";
import { Chip } from "../../wardyn/primitives";
import { fmtAmount, fmtInUse, RES_KEYS, RES_NAME, usedPct, type ResView } from "./resources";

const HEAD = "border-b border-border py-1 pr-2 text-left align-bottom text-meta font-semibold text-muted-foreground";
const CELL = "border-b border-border py-1 pr-2 align-middle max-sm:block max-sm:border-0 max-sm:p-0";
const STACK_LABEL = "block label-eyebrow sm:hidden";

function Bar({ pct }: { pct: number }) {
  return (
    <span role="img" aria-label={RES.PCT_IN_USE(pct)} className="mt-1 block h-1.5 overflow-hidden rounded-full border border-border bg-muted">
      <span className={`block h-full ${pct >= 90 ? "bg-warning" : "bg-info"}`} style={{ width: `${pct}%` }} />
    </span>
  );
}

export function ResourceTable({ view, pool = false }: { view: ResView; pool?: boolean }) {
  const perRun = pool ? RES.BIGGEST_RUN : RES.PER_RUN;
  return (
    <table data-res className="w-full table-fixed border-collapse text-xs max-sm:block">
      <thead className="max-sm:hidden">
        <tr>
          <th className={`${HEAD} w-16`} />
          <th className={HEAD}>{RES.TOTAL}</th>
          <th className={HEAD}>{perRun}</th>
          <th className={HEAD}>{RES.IN_USE}</th>
          <th className={HEAD}>{RES.AVAILABLE}</th>
        </tr>
      </thead>
      <tbody className="max-sm:block">
        {RES_KEYS.map((k) => {
          const r = view.rows[k];
          const full = r.available === 0 && r.total > 0;
          return (
            <tr key={k} data-r={k} className="max-sm:grid max-sm:grid-cols-2 max-sm:gap-x-3 max-sm:gap-y-1 max-sm:border-b max-sm:border-border max-sm:py-1.5">
              <th scope="row" className="border-b border-border py-1 pr-2 text-left align-middle font-medium text-foreground max-sm:col-span-2 max-sm:block max-sm:border-0 max-sm:p-0 max-sm:font-semibold">
                {RES_NAME[k]}
              </th>
              <td className={CELL}>
                <span className={STACK_LABEL}>{RES.TOTAL}</span>
                {fmtAmount(k, r.total)}
              </td>
              <td className={CELL}>
                <span className={STACK_LABEL}>{perRun}</span>
                {r.perRun === null ? <span className="text-muted-foreground">—</span> : fmtAmount(k, r.perRun)}
              </td>
              <td className={CELL}>
                <span className={STACK_LABEL}>{RES.IN_USE}</span>
                <span className="block whitespace-nowrap">{fmtInUse(k, r)}</span>
                <Bar pct={usedPct(r)} />
              </td>
              <td className={CELL}>
                <span className={STACK_LABEL}>{RES.AVAILABLE}</span>
                {!view.counted ? (
                  <span className="text-muted-foreground">{RES.NOT_COUNTED}</span>
                ) : full ? (
                  <span className="inline-flex flex-wrap items-center gap-1.5">
                    {fmtAmount(k, 0)} <Chip tone="warning">{RES.FULL}</Chip>
                  </span>
                ) : (
                  fmtAmount(k, r.available)
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

/** "Runs at once: 3 · Running now: 2 of 3", with the chip when every slot is taken. */
export function RunsLine({ max, active }: { max: number; active: number }) {
  return (
    <p data-runs-line className="mb-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-body text-foreground">
      <strong className="font-semibold">{RES.RUNS_AT_ONCE(max)}</strong>
      <span aria-hidden>·</span>
      {RES.RUNNING_NOW(active, max)}
      {active >= max && <Chip tone="warning">{RES.AT_LIMIT}</Chip>}
    </p>
  );
}

/** Today's offline note, or for a runner that registered but never connected, where to look on its computer. */
export function OfflineNote({ neverConnected = false }: { neverConnected?: boolean }) {
  return (
    <div className="mt-2" data-offline-note>
      <Notice tone="warning" live>
        {neverConnected
          ? RES.NEVER_CONNECTED_NOTE.map((part, i) => (i % 2 ? <code key={i} className="font-mono">{part}</code> : part))
          : RES.OFFLINE_NOTE}
      </Notice>
    </div>
  );
}

/**
 * Why the runner last failed to connect, start or reach its run relay, as the runner reported it: the
 * sentence for its stage and when. Warning while the failure is going on, else informational.
 */
export function FailureNote({ failure }: { failure?: RunnerFailure }) {
  if (!failure) return null;
  const sentence = RES.FAILURE_STAGE[failure.stage];
  if (!sentence) return null;
  return (
    <div className="mt-2" data-failure-note>
      <Notice tone={failure.ongoing ? "warning" : "info"} live>
        {failure.ongoing ? RES.FAILURE_ONGOING(sentence) : RES.FAILURE_LAST(sentence, relativeTime(failure.at))}
      </Notice>
    </div>
  );
}

/** A green or grey dot before a runner's name, with the word for assistive technology. */
export function OnlineDot({ online }: { online: boolean }) {
  return (
    <>
      <span aria-hidden className={`mr-1.5 inline-block size-2 rounded-full align-middle ${online ? "bg-success" : "bg-muted-foreground"}`} />
      <span className="sr-only">{online ? RUNNER.ONLINE : RUNNER.OFFLINE}</span>
    </>
  );
}

export const kindWord = (kind: RunnerKind) => (kind === "self_hosted" ? RUNNER_KIND.SELF : RUNNER_KIND.REMOTE);

export function KindChip({ kind }: { kind: RunnerKind }) {
  return <Chip>{kindWord(kind)}</Chip>;
}

/** A drawer or card section: an eyebrow, an optional right-hand note, and its content. */
export function Sec({ label, meta, children, ...rest }: { label: string; meta?: React.ReactNode; children: React.ReactNode } & React.HTMLAttributes<HTMLElement>) {
  return (
    <section className="border-t border-border py-3 first:border-t-0" {...rest}>
      <div className="mb-1.5 flex flex-wrap items-baseline gap-2">
        <h3 className="label-eyebrow">{label}</h3>
        {meta && <span className="ml-auto text-meta text-muted-foreground">{meta}</span>}
      </div>
      <div className="text-body text-foreground">{children}</div>
    </section>
  );
}

export const Hint = ({ children, ...rest }: React.HTMLAttributes<HTMLParagraphElement>) => (
  <p className="mt-1.5 text-xs text-muted-foreground" {...rest}>
    {children}
  </p>
);
