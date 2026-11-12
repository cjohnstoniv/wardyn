/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the run page says about a macOS VM beyond a Linux one's desktop block (mock M-D1): that the Mac only
// lets the desktop be watched, and where the removal of the VM's disk stands at the run's end. The view-only
// desktop comes from GET /runs/{id}'s macos (macOSRunFactsOf); the disk's removal from GET /runs/{id}'s macos_disk.
import { Check } from "lucide-react";
import { shortTime } from "../../../lib/format";
import type { RunDetail } from "../../../lib/types";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { KEEP_LIST, KEEP_RUN } from "../../wardyn/copy/keep-machine";
import { RUN_VM_PAGE } from "../../wardyn/copy/run-cockpit";

export interface MacOSRunFacts {
  /** The Mac's Screen Sharing is view only: the desktop is watched, with no Open desktop and no sign-ins. */
  desktop?: "observe";
  /** The helper's word on the VM's disk; absent while the run goes on. */
  disk?: { state: "removing" } | { state: "removed"; at: string } | { state: "failed"; reason: string } | { state: "kept"; mac: string; name: string };
}

/** The runner's reason codes MV-D12 names (runner.MacOSVMDiskHelperAlive and its sibling); any other is MV-D12c. */
export const DISK_REASONS: ReadonlyMap<string, string> = new Map([
  ["helper_alive", DESKTOP.diskReasonInUse],
  ["remove_failed", DESKTOP.diskReasonDeleteFailed],
]);

/** The facts GET /runs/{id} carries for a macOS VM run: a desktop the Mac lets be watched alone, and the disk at the end. */
export const macOSRunFactsOf = (run: Pick<RunDetail, "macos" | "macos_disk" | "machine">): MacOSRunFacts | undefined => {
  const facts: MacOSRunFacts = {};
  if (run.macos?.desktop === "observe") facts.desktop = "observe";
  const d = run.macos_disk;
  if (d?.state === "removing") facts.disk = { state: "removing" };
  else if (d?.state === "removed" && d.at) facts.disk = { state: "removed", at: shortTime(d.at) };
  // The disk was kept on purpose: the run keeps its machine (K-43). Said only with the machine to name; never a failed removal.
  else if (d?.state === "kept" && d.reason === "kept_machine") {
    if (run.machine?.name) facts.disk = { state: "kept", mac: run.machine.runner_name || KEEP_LIST.THIS_MAC, name: run.machine.name };
  } else if (d?.state === "kept") facts.disk = { state: "failed", reason: DISK_REASONS.get(d.reason ?? "") ?? DESKTOP.diskReasonUnknown };
  return facts.desktop || facts.disk ? facts : undefined;
};

/** The run page keeps polling a finished run while its macOS VM disk is still being removed. */
export const macOSDiskPending = (run: Pick<RunDetail, "macos_disk"> | null | undefined): boolean => run?.macos_disk?.state === "removing";

export function MacOSDiskNote({ disk }: { disk: MacOSRunFacts["disk"] }) {
  if (!disk) return null;
  if (disk.state === "removing") return <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{DESKTOP.diskRemoving}</p>;
  if (disk.state === "kept") return <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{KEEP_RUN.DISK_KEPT(disk.mac, disk.name)}</p>;
  if (disk.state === "failed") return <p role="alert" className="mt-2 text-meta leading-relaxed text-danger">{DESKTOP.diskRemoveFailed(disk.reason)} {RUN_VM_PAGE.diskKeptNextStep}</p>;
  return (
    <p className="mt-2 flex items-start gap-1.5 text-meta leading-relaxed text-muted-foreground">
      <Check aria-hidden="true" className="mt-0.5 size-3 shrink-0 text-success" />
      {DESKTOP.diskRemoved(disk.at)}
    </p>
  );
}
