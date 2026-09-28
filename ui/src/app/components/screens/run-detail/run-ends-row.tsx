/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the run page's own "Run limits" row (long-holds
// design rev 4 §2.3, mock "Ends": the four states). Extending within the
// run's captured max is always allowed (it IS the lease); shortening,
// picking a date, No end, and the wait need the captured user_changes_limits
// gate — this component reads that gate off run.run_limits, never re-derives
// it, and PATCH /runs/{id} (runs.ts's setRunEndAndWait) is the one source of
// truth for whether an ask actually lands: a capped or refused response is
// rendered from what the server sent back, not guessed client-side.
//
// Self-contained (the LaunchWarningsNote/TerminalPane pattern): owns its own
// PATCH calls and dialog state so run-detail.tsx, already at the file-size
// gate's ceiling, needs only the one mount line.
import * as React from "react";
import { toast } from "sonner";
import type { RunDetail } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { relativeTime, getErrorMessage } from "../../../lib/format";
import { useOperator } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import * as RL from "../../wardyn/copy/run-lifetime";

const DAY_MS = 24 * 60 * 60 * 1000;
const WEEK_MS = 7 * DAY_MS;
// Stands in for "no cap at all" on an uncapped extend — the server clamps to
// the run's real captured max_end_ahead_sec (or the deployment's own ceiling)
// and answers with the actual date, which is what renders afterwards.
const FAR_FUTURE_MS = 100 * 365 * DAY_MS;

// "Tue 18:00" — the mock's own terse date shape for an end time, distinct
// from format.ts's absoluteTime (full date, used for audit timestamps) and
// relativeTime (the app's standard "in 8h" short form, used here beside it).
export function weekdayClock(iso: string): string {
  const d = new Date(iso);
  const weekday = d.toLocaleDateString(undefined, { weekday: "short" });
  const clock = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  return `${weekday} ${clock}`;
}

// <input type=datetime-local> wants local wall-clock time with no offset.
function toDatetimeLocalValue(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function RunEndsRow({ run, onChanged }: { run: RunDetail; onChanged: () => void }) {
  const [changeOpen, setChangeOpen] = React.useState(false);
  const [changeValue, setChangeValue] = React.useState("");
  const [noEnd, setNoEnd] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // handleSetRunEndAndWait's own exempt rule (run_end_wait.go): a super admin
  // is bounded only by the deployment ceiling REGARDLESS of what the run's
  // OWN captured run_limits say — an operator viewing a member's run (or
  // their own, which typically captures no profile at all) is never gated.
  // useOperator, not run.run_limits alone: an operator's own run often comes
  // back with an all-zero-value run_limits OBJECT rather than none at all, so
  // "is run_limits present" cannot stand in for "does a gate apply here". Read
  // unconditionally, ahead of the early return below (Rules of Hooks).
  const operator = useOperator();

  // The Ends row is the LIVE-run surface; a lease-ended or otherwise lost run
  // (kept, no network) and every terminal state get the lifetime banner's own
  // Ended/Lost treatment instead (run-lifetime-banner.tsx) — showing both
  // would tell two different stories about the same run.
  if (isTerminalRunState(run.state) || run.lost_at) return null;

  const limits = run.run_limits;
  const mayChange = operator || (limits ? !!limits.user_changes_limits : true);
  const allowNoEnd = operator || (limits ? !!limits.allow_no_end : true);
  const maxDays = limits?.max_end_ahead_sec ? Math.ceil(limits.max_end_ahead_sec / (24 * 3600)) : undefined;

  const applyEndsAt = async (endsAt: string | null) => {
    setBusy(true);
    try {
      const res = await runsApi.setRunEndAndWait(run.id, { endsAt });
      if (res.capped.includes("ends_at") && res.latest_end) {
        toast.warning(RL.endsCapped(Math.ceil((Date.parse(res.latest_end) - Date.now()) / DAY_MS)));
      }
      onChanged();
    } catch (err) {
      toast.error("Couldn't change this run's end", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const extendBy = (ms: number) => {
    const base = run.ends_at ? Date.parse(run.ends_at) : Date.now();
    void applyEndsAt(new Date(base + ms).toISOString());
  };

  const submitChange = () => {
    setChangeOpen(false);
    if (noEnd) {
      void applyEndsAt(null);
      return;
    }
    if (!changeValue) return;
    void applyEndsAt(new Date(changeValue).toISOString());
  };

  const setWaitHours = async (hours: number) => {
    setBusy(true);
    try {
      const res = await runsApi.setRunEndAndWait(run.id, { waitBudgetSec: Math.round(hours * 3600) });
      if (res.capped.includes("wait_budget_sec")) toast.warning(`Capped at ${res.max_wait_sec ?? hours} sec`);
      onChanged();
    } catch (err) {
      toast.error("Couldn't change the wait", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid="run-ends-row" className="flex flex-col gap-2 border-b border-border px-4 py-2.5 text-xs sm:flex-row sm:flex-wrap sm:items-center sm:gap-4">
      <div className="flex items-center gap-2">
        <span className="font-medium text-foreground">
          {run.ends_at ? RL.endsValue(weekdayClock(run.ends_at), relativeTime(run.ends_at)) : RL.ENDS_NONE}
        </span>
        {run.ends_at ? (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="outline" size="sm" className="h-6 px-2 text-xs" disabled={busy}>
                {RL.ENDS_EXTEND}
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              <DropdownMenuItem onSelect={() => extendBy(DAY_MS)}>{RL.ENDS_EXTEND_1_DAY}</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => extendBy(WEEK_MS)}>{RL.ENDS_EXTEND_1_WEEK}</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => extendBy(FAR_FUTURE_MS)}>
                {maxDays ? RL.endsExtendAsFarAsAllowed(maxDays) : RL.ENDS_EXTEND}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        ) : (
          mayChange && (
            <Button variant="ghost" size="sm" className="h-6 px-2 text-xs" onClick={() => { setNoEnd(false); setChangeOpen(true); }}>
              {RL.ENDS_SET_AN_END}
            </Button>
          )
        )}
        {mayChange ? (
          run.ends_at && (
            <Button variant="ghost" size="sm" className="h-6 px-2 text-xs" onClick={() => { setNoEnd(false); setChangeOpen(true); }}>
              {RL.ENDS_CHANGE}
            </Button>
          )
        ) : (
          <span className="text-muted-foreground">
            {run.ends_at && maxDays ? RL.endsLocked(weekdayClock(run.ends_at), maxDays) : maxDays ? RL.endsHint(maxDays) : null}
          </span>
        )}
      </div>

      {typeof run.wait_budget_sec === "number" && (
        <div className="flex items-center gap-2">
          <span className="text-muted-foreground">{RL.WAIT_LABEL}:</span>
          {mayChange ? (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm" className="h-6 px-2 text-xs" disabled={busy}>
                  {RL.waitValueHours(Math.round(run.wait_budget_sec / 3600))}
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start">
                {[1, 4, 8, 24].map((h) => (
                  <DropdownMenuItem key={h} onSelect={() => void setWaitHours(h)}>
                    {RL.waitValueHours(h)}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
          ) : (
            <span className="text-muted-foreground">{RL.waitLocked(Math.round(run.wait_budget_sec / 3600))}</span>
          )}
        </div>
      )}

      <Dialog open={changeOpen} onOpenChange={setChangeOpen}>
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
                value={changeValue || toDatetimeLocalValue(new Date(Date.now() + DAY_MS))}
                onChange={(e) => setChangeValue(e.target.value)}
              />
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setChangeOpen(false)}>
              Cancel
            </Button>
            <Button onClick={submitChange} disabled={busy}>
              Save
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
