/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "macOS virtual machines" section of a Mac runner's drawer, and its card badge (mocks M-R1, M-R2).
// Only what the runner's caps say: off (a Mac that sends none), preparing with its percent, ready, update
// needed, failed, and not offered (its refusal, or a per-run size under the base's minimum). Both take the
// caps (RunnerView.macos_vm) as a prop, and `active` is the runner's macOS VM runs now (usage.in_use.macos_vm_runs).
import * as React from "react";
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { RunnerAmounts } from "../../../lib/types/runners";
import { MACVM_P, MACVM_R } from "../../wardyn/copy/runner-macvm";
import { harnessDisplayNames } from "../../../lib/model-access";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { RES } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { Hint } from "./resource-table";
import { shortDate } from "../../../lib/format";
import { baseAgeOf, editedAtText, macVMBadge, postureRows, refused, sizeShortfall } from "./macvm-view";

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
  const { status: setup } = useShellSetupStatus();
  const harnesses = caps.harnesses ?? [];
  const rows: [string, React.ReactNode][] = [];
  if (guest_os) rows.push([MACVM_R.R20, guest_os]);
  if (active !== undefined && state === "ready") rows.push(["", RES.RUNNING_NOW(active, caps.max)]);
  if (caps.min_cpus || caps.min_memory_mib) {
    const parts = [caps.min_cpus && RES.CPUS(caps.min_cpus), caps.min_memory_mib && `${RES.GB(caps.min_memory_mib / 1024)} memory`].filter(Boolean);
    rows.push([MACVM_R.R21, parts.join(" · ")]);
  }
  if (harnesses.length) rows.push([MACVM_R.R22, harnesses.map((h) => harnessDisplayNames(setup, [h])).join(", ")]);
  if (caps.desktop === "control") rows.push([MACVM_R.R23, MACVM_R.R24]);
  if (caps.base.edited_at) rows.push([MACVM_P.E16, MACVM_P.E17(editedAtText(caps.base.edited_at))]);
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

/** The base's age, and beside it the Mac's own macOS against the guest's (MAC-P09 to P11). */
function BaseAge({ age, host, guest }: { age: NonNullable<ReturnType<typeof baseAgeOf>>; host: string | undefined; guest: string | undefined }) {
  return (
    <dl className="mt-1 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
      <dt className="text-muted-foreground">{MACVM_P.P09}</dt>
      <dd>
        {MACVM_P.P10(shortDate(age.created), age.days)}
        {host && guest && <span className="block text-meta text-muted-foreground">{MACVM_P.P11(host, guest)}</span>}
      </dd>
    </dl>
  );
}

/** What the runner read of the guest in the base, as it was when the base was prepared or edited (MAC-P01 to P08). */
function GuestPosture({ rows }: { rows: [string, string][] }) {
  if (!rows.length) return null;
  return (
    <div data-guest-posture>
      <h4 className="mt-2 text-meta font-medium text-foreground">{MACVM_P.P01}</h4>
      <dl className="mt-1 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        {rows.map(([label, value]) => (
          <React.Fragment key={label}>
            <dt className="text-muted-foreground">{label}</dt>
            <dd>{value}</dd>
          </React.Fragment>
        ))}
      </dl>
      <Hint>{MACVM_P.P08}</Hint>
    </div>
  );
}

function Body({ caps, active, perRun, runner, hostOS, maxBaseAgeDays }: { caps: MacVMCaps; active: number | undefined; perRun?: RunnerAmounts; runner: string; hostOS?: string; maxBaseAgeDays?: number }) {
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
  if (state !== "ready" || refused(caps) || short.memory || short.cpu) {
    return (
      <>
        <Chip>{MACVM_R.R14}</Chip>
        {caps.reason === "ledger_damaged" && <p className="mt-1 text-foreground">{MACVM_R.LEDGER_DAMAGED}</p>}
        {short.memory && <p className="mt-1 text-foreground"><MacVMText text={MACVM_R.R47(fmt(short.memory.have), fmt(short.memory.need), runner)} /></p>}
        {short.cpu && <p className={short.memory ? "mt-1 text-xs text-muted-foreground" : "mt-1 text-foreground"}><MacVMText text={MACVM_R.R48(fmt(short.cpu.have), fmt(short.cpu.need), runner)} /></p>}
      </>
    );
  }
  const age = baseAgeOf(caps.base, maxBaseAgeDays);
  const ageLine = age && <BaseAge age={age} host={hostOS} guest={caps.base.guest?.os_version} />;
  const posture = <GuestPosture rows={postureRows(caps.base.guest)} />;
  // An organisation maximum the base is past: not offered, and the Mac's owner is told what to run. Age and posture still show.
  if (age?.tooOld) {
    return (
      <>
        <div className="flex flex-wrap items-center gap-2">
          <Chip>{MACVM_R.R14}</Chip>
          <Chip tone="danger">{MACVM_P.P12}</Chip>
          {guest_os && <span className="text-muted-foreground">{MACVM_R.R06(guest_os)}</span>}
        </div>
        <p className="mt-1 text-foreground"><MacVMText text={MACVM_P.P13(age.days, age.max!)} /></p>
        {ageLine}
        {posture}
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
      {ageLine}
      {posture}
      <div className="mt-1"><Facts caps={caps} active={active} /></div>
      {!observe && <Hint>{MACVM_R.R25}</Hint>}
    </>
  );
}

/** The drawer section. `mac` says the runner reports a macOS host: without caps that reads as off, and any other runner shows nothing. */
export function MacVMSection({ caps, mac, active, perRun, runner, hostOS, maxBaseAgeDays }: {
  caps: MacVMCaps | undefined;
  mac: boolean;
  active?: number;
  perRun?: RunnerAmounts;
  runner: string;
  /** The Mac's own macOS version, from its posture. */
  hostOS?: string;
  /** The organisation's maximum base age in days (site config macos_vm.max_base_age_days); absent or 0 is none. */
  maxBaseAgeDays?: number;
}) {
  if (!caps && !mac) return null;
  return (
    <section className="border-b border-border py-3" data-macvm>
      <div className="mb-1 flex items-baseline justify-between gap-3">
        <h3 className="label-eyebrow">{MACVM_R.R00}</h3>
        <span className="text-meta text-muted-foreground">{RUNNER.REPORTED}</span>
      </div>
      <div className="space-y-0.5 text-body text-foreground">
        {caps ? (
          <Body caps={caps} active={active} perRun={perRun} runner={runner} hostOS={hostOS} maxBaseAgeDays={maxBaseAgeDays} />
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
