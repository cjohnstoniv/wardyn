/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the "Change…" dialog (mock "Ends"), shared by the
// Ends row's own Change…/Set an end… buttons and the warning banner's "Change
// end…" (M1, PR #1317 review) so there is exactly one Change flow, not two
// copies that could drift. F7 (PR #1317 review): the datetime-local input's
// value is seeded into REAL state the moment the dialog opens (not derived at
// render time with a `|| default` fallback) — Save with the default left
// untouched used to send nothing at all, because the input's DISPLAYED value
// and the state Save reads were two different things.
import * as React from "react";
import { toast } from "sonner";
import type { RunDetail } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import * as RL from "../../wardyn/copy/run-lifetime";

const DAY_MS = 24 * 60 * 60 * 1000;

// <input type=datetime-local> wants local wall-clock time with no offset.
function toDatetimeLocalValue(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function ChangeEndDialog({
  open,
  onOpenChange,
  run,
  onChanged,
  maxDays,
  allowNoEnd,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  run: RunDetail;
  onChanged: () => void;
  maxDays?: number;
  allowNoEnd: boolean;
}) {
  const [changeValue, setChangeValue] = React.useState(() => toDatetimeLocalValue(new Date(Date.now() + DAY_MS)));
  const [noEnd, setNoEnd] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // A fresh default every time the dialog OPENS — "now + 1 day" from the
  // moment it opens, not from whenever this component first mounted.
  React.useEffect(() => {
    if (open) {
      setChangeValue(toDatetimeLocalValue(new Date(Date.now() + DAY_MS)));
      setNoEnd(false);
    }
  }, [open]);

  const submit = async () => {
    setBusy(true);
    try {
      const endsAt = noEnd ? null : new Date(changeValue).toISOString();
      const res = await runsApi.setRunEndAndWait(run.id, { endsAt });
      if (res.capped.includes("ends_at") && res.latest_end) {
        toast.warning(RL.endsCapped(Math.ceil((Date.parse(res.latest_end) - Date.now()) / DAY_MS)));
      }
      onChanged();
      onOpenChange(false);
    } catch (err) {
      toast.error("Couldn't change this run's end", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{RL.ENDS_CHANGE}</DialogTitle>
          <DialogDescription>{maxDays ? RL.endsHint(maxDays) : "Pick a date, or turn this run's end off."}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          {allowNoEnd && (
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={noEnd} onChange={(e) => setNoEnd(e.target.checked)} />
              {RL.ENDS_NO_END_OPTION}
            </label>
          )}
          {!noEnd && (
            <input
              type="datetime-local"
              aria-label="Ends at"
              className="rounded-md border border-border-strong bg-background px-2 py-1.5 text-sm"
              value={changeValue}
              onChange={(e) => setChangeValue(e.target.value)}
            />
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={() => void submit()} disabled={busy}>
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
