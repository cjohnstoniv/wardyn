/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A lane of a helper's run page whose door the owner did not tick: its heading kept, one sentence (P-01) and no
// button. The server refuses the same attempt with support_door_not_granted; this says so before the click.
import type { SupportDoor } from "../../../lib/api/support";
import { SUPPORT_HELPER } from "../../wardyn/copy/support";

/** P-01 as a line, for a lane that keeps its own heading (the SSH lane's summary, the terminal pane). */
export function ClosedDoorsLine({ doors, className = "mt-0.5" }: { doors: readonly SupportDoor[]; className?: string }) {
  return (
    <p data-testid="helper-doors-closed" className={`${className} text-meta leading-relaxed text-muted-foreground`}>
      {SUPPORT_HELPER.doorsClosed(doors)}
    </p>
  );
}

/** A whole closed lane: its heading, then P-01. */
export function ClosedLane({ title, doors, className = "mt-4 border-t border-border pt-3" }: { title: string; doors: readonly SupportDoor[]; className?: string }) {
  return (
    <div className={className}>
      <p className="text-xs font-medium text-foreground">{title}</p>
      <ClosedDoorsLine doors={doors} />
    </div>
  );
}
