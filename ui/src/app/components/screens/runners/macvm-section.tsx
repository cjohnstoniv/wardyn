/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "macOS virtual machines" section of a Mac runner's drawer, and its card badge (mocks M-R1, M-R2).
// Only what the runner's caps say: off (a Mac that sends none), preparing with its percent, ready, update
// needed, failed, and not offered (its refusal, or a per-run size under the base's minimum). Both take the
// caps as a prop: RunnerView carries none yet. `active` is the runner's macOS VM runs now, which no wire
// field gives per kind either.
import * as React from "react";
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { RunnerAmounts } from "../../../lib/types/runners";
import { MACVM_R } from "../../wardyn/copy/runner-macvm";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { RES } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { Hint } from "./resource-table";
import { macVMBadge, sizeShortfall } from "./macvm-view";

/** Harness id to the name the New Run harness picker shows. */
const HARNESS_NAME: Record<string, string> = { "claude-code": "Claude Code", "codex-cli": "Codex CLI", none: "Your own tools" };

/** A string with `code` spans, the way the boards set `wardyn-runnerd setup`. */
export function MacVMText({ text }: { text: string }) {
  return (
    <>
      {text.split("`").map((part, i) => (i % 2 ? <Mono key={i}>{part}</Mono> : <React.Fragment key={i}>{part}</React.Fragment>))}
    </>
  );
}

export function MacVMBadge({ caps, active, perRun }: { caps: MacVMCaps | undefined; active: number | undefined; perRun?: RunnerAmounts }) {
  const badge = macVMBadge(caps, active, perRun);
  return badge ? <Chip tone={badge.tone}>{badge.text}</Chip> : null;
}

const fmt = (n: number) => String(Number(n.toFixed(1)));

function Facts({ caps, active }: { caps: MacVMCaps; active: number | undefined }) {
  const { guest_os, state } = caps.base;
  const harnesses = caps.harnesses ?? [];
  const rows: [string, React.ReactNode][] = [];
  if (guest_os) rows.push([MACVM_R.R20, guest_os]);
  if (active !== undefined && state === "ready") rows.push(["", RES.RUNNING_NOW(active, caps.max)]);
  if (caps.min_cpus || caps.min_memory_mib) {
    const parts = [caps.min_cpus && RES.CPUS(caps.min_cpus), caps.min_memory_mib && `${RES.GB(caps.min_memory_mib / 1024)} memory`].filter(Boolean);
    rows.push([MACVM_R.R21, parts.join(" · ")]);
  }
  if (harnesses.length) rows.push([MACVM_R.R22, harnesses.map((h) => HARNESS_NAME[h] ?? h).join(", ")]);
  if (caps.desktop === "control") rows.push([MACVM_R.R23, MACVM_R.R24]);
  return (
    <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
      {rows.map(([label, value], i) => (
        <React.Fragment key={i}>
          <dt className="text-muted-foreground">{label}</dt>
          <dd>{value}</dd>
        </React.Fragment>
      ))}
    </dl>
  );
}

function Body({ caps, active, perRun, runner }: { caps: MacVMCaps; active: number | undefined; perRun?: RunnerAmounts; runner: string }) {
  const { state, progress, guest_os, error } = caps.base;
  const short = sizeShortfall(caps, perRun);
  if (state === "preparing") {
    const pct = progress ?? 0;
    return (
      <>
        <div className="flex items-center gap-2">
          <Chip tone="info" dot pulse>{MACVM_R.R10}</Chip>
        </div>
        <span role="img" aria-label={`${pct}%`} className="mt-1 block h-1.5 overflow-hidden rounded-full border border-border bg-muted">
          <span className="block h-full bg-info" style={{ width: `${pct}%` }} />
        </span>
      </>
    );
  }
  if (state === "update_needed") {
    return (
      <>
        <Chip tone="warning" dot>{MACVM_R.R17}</Chip>
        <p className="mt-1 text-warning">{MACVM_R.R43}</p>
      </>
    );
  }
  if (state === "failed") {
    return (
      <>
        <Chip tone="danger">{MACVM_R.R15}</Chip>
        {error && <p className="mt-1 text-danger">{error}</p>}
      </>
    );
  }
  if (state !== "ready" || caps.refusal || short.memory || short.cpu) {
    return (
      <>
        <Chip>{MACVM_R.R14}</Chip>
        {caps.refusal && <p className="mt-1 text-foreground">{caps.refusal}</p>}
        {short.memory && <p className="mt-1 text-foreground"><MacVMText text={MACVM_R.R47(fmt(short.memory.have), fmt(short.memory.need), runner)} /></p>}
        {short.cpu && <p className={short.memory ? "mt-1 text-xs text-muted-foreground" : "mt-1 text-foreground"}><MacVMText text={MACVM_R.R48(fmt(short.cpu.have), fmt(short.cpu.need), runner)} /></p>}
      </>
    );
  }
  const observe = caps.desktop === "observe";
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="success" dot>{DESKTOP.ready}</Chip>
        {observe && <Chip>{MACVM_R.R19}</Chip>}
        {guest_os && <span className="text-muted-foreground">{MACVM_R.R06(guest_os)}</span>}
      </div>
      {observe && <p className="mt-1 text-foreground">{MACVM_R.R40}</p>}
      <div className="mt-1"><Facts caps={caps} active={active} /></div>
      {!observe && <Hint>{MACVM_R.R25}</Hint>}
    </>
  );
}

/** The drawer section. `mac` says the runner reports a macOS host: without caps that reads as off, and any other runner shows nothing. */
export function MacVMSection({ caps, mac, active, perRun, runner }: { caps: MacVMCaps | undefined; mac: boolean; active?: number; perRun?: RunnerAmounts; runner: string }) {
  if (!caps && !mac) return null;
  return (
    <section className="border-b border-border py-3" data-macvm>
      <div className="mb-1 flex items-baseline justify-between gap-3">
        <h3 className="label-eyebrow">{MACVM_R.R00}</h3>
        <span className="text-meta text-muted-foreground">{RUNNER.REPORTED}</span>
      </div>
      <div className="space-y-0.5 text-body text-foreground">
        {caps ? (
          <Body caps={caps} active={active} perRun={perRun} runner={runner} />
        ) : (
          <>
            <Chip>{DESKTOP.statusOff}</Chip>
            <p className="mt-1 text-muted-foreground"><MacVMText text={MACVM_R.R30} /></p>
          </>
        )}
      </div>
    </section>
  );
}
