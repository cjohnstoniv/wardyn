/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the run page says about a macOS VM beyond a Linux one's desktop block (mock M-D1): that the Mac only
// lets the desktop be watched, and where the removal of the VM's disk stands at the run's end. The view-only
// desktop comes from GET /runs/{id}'s macos (macOSRunFactsOf); the disk's removal has no wire field yet, so it
// enters as a prop and the block says nothing of it without one.
import { Check } from "lucide-react";
import type { RunDetail } from "../../../lib/types";
import { DESKTOP } from "../../wardyn/copy/desktop";

export interface MacOSRunFacts {
  /** The Mac's Screen Sharing is view only: the run is agent-only, with no Open desktop. */
  desktop?: "observe";
  /** The helper's word on the VM's disk; absent while the run goes on. */
  disk?: { state: "removing" } | { state: "removed"; at: string } | { state: "failed"; reason: string };
}

/** The facts GET /runs/{id} carries for a macOS VM run: today only a desktop the Mac lets be watched alone. */
export const macOSRunFactsOf = (run: Pick<RunDetail, "macos">): MacOSRunFacts | undefined =>
  run.macos?.desktop === "observe" ? { desktop: "observe" } : undefined;

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
