/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Segmented } from "../../../../wardyn/segmented";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_WINDOW } from "../../../../wardyn/copy/new-run/window";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { isVMKind, macOSVMsEnabled, type MachineKind } from "../../../../../lib/types/machine-kind";
import { Button } from "../../../../ui/button";
import { KEEP_NEW_RUN } from "../../../../wardyn/copy/keep-machine";
import type { RunIntent } from "../../draft/run-intent";
import type { KeptChoice } from "./kept-machine-choice";
import { KeptMachineSection } from "./kept-machine-section";
import { MacOSBasePicker } from "./macos-base-picker";
import { hasBaseChoice } from "./macos-bases";
import { refusalText, useMacOSOffer, type MacOSOffer } from "./macos-offer";
import { POOL_ID } from "./pool-section";
import { macWindowOn } from "./window-choice";
import { WindowOption } from "./window-option";

const note = "text-xs leading-snug text-muted-foreground";

/** A line with `code` spans drawn in the mono face, as the refusals name a command or a path. */
const withCode = (text: string) => text.split("`").map((part, i) => (i % 2 ? <code key={i} className="font-mono text-foreground">{part}</code> : part));

/**
 * The first row of "Where should it run?" (mock M5-Revised): the machine kind, then for a VM its operating
 * system and the fixed barrier line, in place of the container barriers below the pool. Linux is always there;
 * macOS joins it while the organisation's switch is on (mock M-N1), disabled with its reason when the pool's
 * Macs cannot take one. A macOS pool with several bases adds the base picker (board J6). A macOS VM adds the window on
 * the Mac while the site switch is on (board WIN-2); with it off there is no Window group at all.
 */
export function MachineKindSection({ kind, agents = [], macosBase, windowChoice, kept, macPools = [], onPickMacPool, onChoose, onChooseBase }: {
  kind: MachineKind;
  /** The other pools that offer macOS now, in the pool list's order: shown under the refusal, each a button. */
  macPools?: readonly { id: string; name: string }[];
  /** Chooses that pool and macOS together. */
  onPickMacPool?: (poolId: string) => void;
  agents?: readonly string[];
  /** The draft's macOS base; absent is Standard. */
  macosBase?: string;
  /** The draft's window on the Mac: chosen, being checked, or cleared by the server with its sentence. */
  windowChoice?: { chosen: boolean; checking: boolean; cleared?: string; onChoose: (on: boolean) => void };
  /** The machine between runs: offered only where `choice` is set (kept-machine-choice.ts). */
  kept?: {
    choice: KeptChoice | undefined;
    /** The read of the kept machines failed where it would have been offered: say so, with this retry. */
    retry?: () => void;
    /** The sentence for a choice a server answer proved invalid and cleared. */
    cleared?: string;
    machine: RunIntent["machine"];
    onDelete: () => void;
    onKeep: () => void;
    onName: (name: string) => void;
    onResume: (id: string) => void;
    onFresh: (fresh: boolean) => void;
  };
  onChoose: (kind: MachineKind) => void;
  onChooseBase: (base: string | undefined) => void;
}) {
  const labelId = React.useId();
  const osRef = React.useRef<HTMLDivElement>(null);
  // A pool button goes away once its pick lifts the refusal; focus follows to the macOS control it chose, or to the pool select.
  const refocus = React.useRef(false);
  const { status } = useShellSetupStatus();
  const offer = useMacOSOffer();
  const macOSOn = macOSVMsEnabled(status);
  // The site's window switch (setup features); absent reads off.
  const windowOn = macWindowOn(status);
  const vm = isVMKind(kind);
  const macOS = kind === "macos_vm";
  const refused = macOSOn && offer && !offer.ok ? refusalText(offer.refusal) : undefined;
  // The draft's first agent whose admin disallows sign-ins in a desktop (the setup status's own harness row):
  // a macOS desktop cannot block them, so the panel says the run will be refused one.
  const signInsOffFor = status?.harnesses?.find((h) => h.desktop_sign_ins_off && agents.includes(h.id))?.display;
  React.useEffect(() => {
    if (!refocus.current) return;
    refocus.current = false;
    const mac = macOS && !refused ? osRef.current?.querySelector<HTMLButtonElement>("button[aria-pressed='true']") : null;
    (mac ?? document.getElementById(POOL_ID))?.focus();
  });
  return (
    <div className="mb-[18px] flex flex-col gap-[18px]">
      <div role="group" aria-labelledby={labelId} className="flex flex-col gap-2">
        <span id={labelId} className="text-xs font-semibold">{NR_MACHINE.LABEL}</span>
        <Segmented
          size="compact"
          value={vm ? "vm" : "container"}
          onChange={(v) => onChoose(v === "vm" ? (macOS ? "macos_vm" : "linux_vm") : "container")}
          options={[{ value: "container", label: NR_MACHINE.CONTAINER }, { value: "vm", label: NR_MACHINE.VM }]}
        />
        <p className={note}>{vm ? NR_MACHINE.VM_HELP : NR_MACHINE.CONTAINER_HELP}</p>
      </div>
      {vm && (
        <>
          <div ref={osRef} role="group" aria-label={NR_MACHINE.OS_LABEL} className="flex flex-col gap-2">
            <span className="text-xs font-semibold" aria-hidden="true">{NR_MACHINE.OS_LABEL}</span>
            <Segmented
              size="compact"
              value={macOS ? "macos_vm" : "linux_vm"}
              onChange={onChoose}
              options={[
                { value: "linux_vm", label: NR_MACHINE.OS_LINUX },
                ...(macOSOn ? [{ value: "macos_vm" as const, label: NR_MACHINE.OS_MACOS, disabled: !!refused }] : []),
              ]}
            />
            {refused && <p className={note}>{withCode(refused)}</p>}
            {refused && macPools.length > 0 && onPickMacPool && (
              <div data-testid="nr-macos-pools" className="flex flex-wrap items-center gap-2">
                <span className={note}>{NR_MACHINE.MACOS_POOLS_OFFER}</span>
                {macPools.map((p) => (
                  <Button key={p.id} type="button" variant="outline" size="sm" className="h-auto min-w-0 whitespace-normal py-1 text-left" aria-label={NR_MACHINE.MACOS_POOL_PICK(p.name)}
                    onClick={() => { refocus.current = true; onPickMacPool(p.id); }}>
                    {p.name}
                  </Button>
                ))}
              </div>
            )}
            <MacOSLines loaded={status != null && !status.unreachable} on={macOSOn} chosen={macOS} offer={offer} windowOn={windowOn} />
          </div>
          {macOS && offer?.ok && !kept?.machine?.resume && hasBaseChoice(offer, macosBase) && <MacOSBasePicker offer={offer} value={macosBase} onChoose={onChooseBase} />}
          {macOS && windowOn && windowChoice && (
            <WindowOption {...windowChoice} runner={offer?.ok ? offer.runner : undefined} />
          )}
          {macOS && kept?.choice && <KeptMachineSection {...kept} choice={kept.choice} />}
          {macOS && kept?.retry && (
            <div role="alert" data-testid="nr-kept-machine-failed" className="flex flex-wrap items-center gap-2 text-xs">
              <span>{KEEP_NEW_RUN.READ_FAILED}</span>
              <Button type="button" variant="outline" size="sm" onClick={kept.retry}>{KEEP_NEW_RUN.RETRY}</Button>
            </div>
          )}
          {macOS && kept?.cleared && <p role="status" data-testid="nr-kept-machine-cleared" className="text-xs text-warning">{kept.cleared}</p>}
          <dl data-testid="nr-vm-barrier" className="grid grid-cols-[96px_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs">
            <dt className="text-muted-foreground">{NR_MACHINE.BARRIER_LABEL}</dt>
            <dd className="font-medium">{NR_MACHINE.BARRIER_VM}</dd>
            <dd className="col-start-2 text-[11px] text-muted-foreground">{NR_MACHINE.BARRIER_VM_WHY}</dd>
          </dl>
          <section aria-label={NR_MACHINE.CHANGES_TITLE} className="rounded-[9px] border border-border bg-surface-2 p-3.5 text-xs">
            <p className="mb-2.5 font-semibold" aria-hidden="true">{NR_MACHINE.CHANGES_TITLE}</p>
            <dl className="grid grid-cols-[96px_minmax(0,1fr)] gap-x-3 gap-y-1.5">
              <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_NETWORK}</dt>
              <dd>{NR_MACHINE.CHANGES_NETWORK_BODY}</dd>
              <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_CLIPBOARD}</dt>
              <dd>{macOS ? NR_MACHINE.CHANGES_CLIPBOARD_MACOS_NONE : NR_MACHINE.CHANGES_CLIPBOARD_BODY}</dd>
              {macOS && <MacOSChanges signInsOffFor={signInsOffFor} keptOn={kept?.choice && kept.machine ? kept.choice.mac : undefined} />}
            </dl>
          </section>
        </>
      )}
    </div>
  );
}

/**
 * What stands under the OS row: Linux's help, the Windows line (or, with macOS off, the line that says so), or for a
 * chosen macOS what it gives and starts with. Before the setup status has loaded the macOS part is unknown, so only the
 * Windows line shows.
 */
function MacOSLines({ loaded, on, chosen, offer, windowOn }: { loaded: boolean; on: boolean; chosen: boolean; offer: MacOSOffer | undefined; windowOn: boolean }) {
  if (!chosen) {
    return (
      <>
        <p className={note}>{NR_MACHINE.OS_LINUX_HELP}</p>
        <p className={note}>{loaded && !on ? NR_WINDOW.OS_LATER : NR_WINDOW.OS_WINDOWS_LATER}</p>
      </>
    );
  }
  return (
    <>
      <p className={note}>{NR_MACHINE.MACOS_GIVES}</p>
      <p className={note}>{windowOn ? NR_WINDOW.VIEW_ONLY_WINDOW_ON : NR_WINDOW.VIEW_ONLY_WINDOW_OFF}</p>
      {offer?.ok && offer.guestOS && <p className={note}>{NR_MACHINE.MACOS_ABOUT(offer.guestOS)}</p>}
      {offer?.ok && <p className={note}>{NR_MACHINE.MACOS_SIZE(String(offer.minCPUs), String(offer.minMemoryMiB))}</p>}
    </>
  );
}

/** The macOS rows of the changes panel (mock M-N2): where sign-ins live (on the kept machine, when the run keeps or resumes one), and the Mac itself. */
function MacOSChanges({ signInsOffFor, keptOn }: { signInsOffFor: string | undefined; keptOn: string | undefined }) {
  return (
    <>
      <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_SIGN_INS}</dt>
      <dd>{signInsOffFor ? NR_MACHINE.CHANGES_SIGN_INS_OFF(signInsOffFor) : keptOn ? KEEP_NEW_RUN.SIGN_INS(keptOn) : NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK}</dd>
      <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_MAC}</dt>
      <dd>{NR_MACHINE.CHANGES_MAC_MANAGED}</dd>
    </>
  );
}
