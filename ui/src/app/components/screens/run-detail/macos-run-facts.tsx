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

export interface MacOSRunFacts {
  /** The Mac's Screen Sharing is view only: the run is agent-only, with no Open desktop. */
  desktop?: "observe";
  /** The helper's word on the VM's disk; absent while the run goes on. */
  disk?: { state: "removing" } | { state: "removed"; at: string } | { state: "failed"; reason: string };
}

/** The runner's reason codes MV-D12 names (runner.MacOSVMDiskHelperAlive and its sibling); any other is MV-D12c. */
export const DISK_REASONS: ReadonlyMap<string, string> = new Map([
  ["helper_alive", DESKTOP.diskReasonInUse],
  ["remove_failed", DESKTOP.diskReasonDeleteFailed],
]);

/** The facts GET /runs/{id} carries for a macOS VM run: a desktop the Mac lets be watched alone, and the disk at the end. */
export const macOSRunFactsOf = (run: Pick<RunDetail, "macos" | "macos_disk">): MacOSRunFacts | undefined => {
  const facts: MacOSRunFacts = {};
  if (run.macos?.desktop === "observe") facts.desktop = "observe";
  const d = run.macos_disk;
  if (d?.state === "removing") facts.disk = { state: "removing" };
  else if (d?.state === "removed" && d.at) facts.disk = { state: "removed", at: shortTime(d.at) };
  else if (d?.state === "kept") facts.disk = { state: "failed", reason: DISK_REASONS.get(d.reason ?? "") ?? DESKTOP.diskReasonUnknown };
  return facts.desktop || facts.disk ? facts : undefined;
};

/** The run page keeps polling a finished run while its macOS VM disk is still being removed. */
export const macOSDiskPending = (run: Pick<RunDetail, "macos_disk"> | null | undefined): boolean => run?.macos_disk?.state === "removing";

export function MacOSDiskNote({ disk }: { disk: MacOSRunFacts["disk"] }) {
  if (!disk) return null;
  if (disk.state === "removing") return <p className="mt-2 text-meta leading-relaxed text-muted-foreground">{DESKTOP.diskRemoving}</p>;
  if (disk.state === "failed") return <p role="alert" className="mt-2 text-meta leading-relaxed text-danger">{DESKTOP.diskRemoveFailed(disk.reason)}</p>;
  return (
    <p className="mt-2 flex items-start gap-1.5 text-meta leading-relaxed text-muted-foreground">
      <Check aria-hidden="true" className="mt-0.5 size-3 shrink-0 text-success" />
      {DESKTOP.diskRemoved(disk.at)}
    </p>
  );
}
