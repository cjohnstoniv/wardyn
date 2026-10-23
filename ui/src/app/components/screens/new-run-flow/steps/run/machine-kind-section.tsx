/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Segmented } from "../../../../wardyn/segmented";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";

export type MachineKind = "container" | "linux_vm";

/**
 * The first row of "Where should it run?" (mock M5-Revised): the machine kind, then for a VM its operating
 * system and the fixed barrier line, in place of the container barriers below the pool. Only Linux VMs exist in
 * this release, so the OS row has one option and one muted line for the rest.
 */
export function MachineKindSection({ kind, onChoose }: { kind: MachineKind; onChoose: (kind: MachineKind) => void }) {
  const labelId = React.useId();
  const vm = kind === "linux_vm";
  return (
    <div className="mb-[18px] flex flex-col gap-[18px]">
      <div role="group" aria-labelledby={labelId} className="flex flex-col gap-2">
        <span id={labelId} className="text-xs font-semibold">{NR_MACHINE.LABEL}</span>
        <Segmented
          size="compact"
          value={kind}
          onChange={onChoose}
          options={[{ value: "container", label: NR_MACHINE.CONTAINER }, { value: "linux_vm", label: NR_MACHINE.VM }]}
        />
        <p className="text-xs leading-snug text-muted-foreground">{vm ? NR_MACHINE.VM_HELP : NR_MACHINE.CONTAINER_HELP}</p>
      </div>
      {vm && (
        <>
          <div role="group" aria-label={NR_MACHINE.OS_LABEL} className="flex flex-col gap-2">
            <span className="text-xs font-semibold" aria-hidden="true">{NR_MACHINE.OS_LABEL}</span>
            <Segmented size="compact" value="linux" onChange={() => undefined} options={[{ value: "linux", label: NR_MACHINE.OS_LINUX }]} />
            <p className="text-xs leading-snug text-muted-foreground">{NR_MACHINE.OS_LATER}</p>
          </div>
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
              <dd>{NR_MACHINE.CHANGES_CLIPBOARD_BODY}</dd>
            </dl>
          </section>
        </>
      )}
    </div>
  );
}
