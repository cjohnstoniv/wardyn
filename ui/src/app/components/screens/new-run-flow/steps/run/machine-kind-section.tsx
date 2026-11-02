/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Segmented } from "../../../../wardyn/segmented";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { isVMKind, macOSVMsEnabled, type MachineKind } from "../../../../../lib/types/machine-kind";
import { MacOSBasePicker } from "./macos-base-picker";
import { hasBaseChoice } from "./macos-bases";
import { refusalText, useMacOSOffer, type MacOSOffer } from "./macos-offer";

const note = "text-xs leading-snug text-muted-foreground";

/** A line with `code` spans drawn in the mono face, as the refusals name a command or a path. */
const withCode = (text: string) => text.split("`").map((part, i) => (i % 2 ? <code key={i} className="font-mono text-foreground">{part}</code> : part));

/**
 * The first row of "Where should it run?" (mock M5-Revised): the machine kind, then for a VM its operating
 * system and the fixed barrier line, in place of the container barriers below the pool. Linux is always there;
 * macOS joins it while the organisation's switch is on (mock M-N1), disabled with its reason when the pool's
 * Macs cannot take one. A macOS pool with several bases adds the base picker (board J6).
 */
export function MachineKindSection({ kind, agents = [], macosBase, onChoose, onChooseBase }: {
  kind: MachineKind;
  agents?: readonly string[];
  /** The draft's macOS base; absent is Standard. */
  macosBase?: string;
  onChoose: (kind: MachineKind) => void;
  onChooseBase: (base: string | undefined) => void;
}) {
  const labelId = React.useId();
  const { status } = useShellSetupStatus();
  const offer = useMacOSOffer();
  const macOSOn = macOSVMsEnabled(status);
  const vm = isVMKind(kind);
  const macOS = kind === "macos_vm";
  const refused = macOSOn && offer && !offer.ok ? refusalText(offer.refusal) : undefined;
  // The draft's first agent whose admin disallows sign-ins in a desktop (the setup status's own harness row):
  // a macOS desktop cannot block them, so the panel says the run will be refused one.
  const signInsOffFor = status?.harnesses?.find((h) => h.desktop_sign_ins_off && agents.includes(h.id))?.display;
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
          <div role="group" aria-label={NR_MACHINE.OS_LABEL} className="flex flex-col gap-2">
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
            <MacOSLines on={macOSOn} chosen={macOS} offer={offer} />
          </div>
          {macOS && offer?.ok && hasBaseChoice(offer, macosBase) && <MacOSBasePicker offer={offer} value={macosBase} onChoose={onChooseBase} />}
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
              {macOS && <MacOSChanges signInsOffFor={signInsOffFor} />}
            </dl>
          </section>
        </>
      )}
    </div>
  );
}

/** What stands under the OS row: the Windows line, or for a chosen macOS what it sets up and starts with. */
function MacOSLines({ on, chosen, offer }: { on: boolean; chosen: boolean; offer: MacOSOffer | undefined }) {
  if (!on) return <p className={note}>{NR_MACHINE.OS_LATER}</p>;
  if (!chosen) return <p className={note}>{NR_MACHINE.OS_WINDOWS_LATER}</p>;
  if (!offer?.ok) return null;
  return (
    <>
      {offer.guestOS && <p className={note}>{NR_MACHINE.MACOS_ABOUT(offer.guestOS)}</p>}
      <p className={note}>{NR_MACHINE.MACOS_SIZE(String(offer.minCPUs), String(offer.minMemoryMiB))}</p>
    </>
  );
}

/** The macOS rows of the changes panel (mock M-N2): where sign-ins live, and the Mac itself. */
function MacOSChanges({ signInsOffFor }: { signInsOffFor: string | undefined }) {
  return (
    <>
      <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_SIGN_INS}</dt>
      <dd>{signInsOffFor ? NR_MACHINE.CHANGES_SIGN_INS_OFF(signInsOffFor) : NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK}</dd>
      <dt className="text-muted-foreground">{NR_MACHINE.CHANGES_MAC}</dt>
      <dd>{NR_MACHINE.CHANGES_MAC_MANAGED}</dd>
    </>
  );
}
