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
import { useOperator, usePrincipal } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import * as RL from "../../wardyn/copy/run-lifetime";
import { ChangeEndDialog } from "./change-end-dialog";

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

export function RunEndsRow({ run, onChanged }: { run: RunDetail; onChanged: () => void }) {
  const [changeOpen, setChangeOpen] = React.useState(false);
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
  // F16 (PR #1317 review): ownsRunOrSuperAdmin is the server's OWN gate on
  // every write here (run_end_wait.go) — a security admin who is not this
  // run's owner and not a true super admin gets a 403 on all of them, so the
  // client must not dangle a control that always fails. usePrincipal, not a
  // view prop: the same predicate run-row.tsx and the terminal pane already
  // use for "own".
  const principal = usePrincipal();

  // The Ends row is the LIVE-run surface; a lease-ended or otherwise lost run
  // (kept, no network) and every terminal state get the lifetime banner's own
  // Ended/Lost treatment instead (run-lifetime-banner.tsx) — showing both
  // would tell two different stories about the same run.
  if (isTerminalRunState(run.state) || run.lost_at) return null;

  const canAct = operator || run.created_by === principal;
  const limits = run.run_limits;
  // F15 (PR #1317 review): AgentRun.RunLimits is a Go VALUE with no
  // omitempty (types.go), so the server ALWAYS sends a run_limits object —
  // never absent. The `limits ? … : true` fallback this used to have was
  // therefore dead code, and wrong in the direction it never ran: an
  // all-absent/zero-value object reads user_changes_limits/allow_no_end as
  // their Go zero (false) on the server, which is LOCKED, not open. A bare
  // `!!limits?.x` agrees with the server on both the reachable case (present,
  // some fields set) and the unreachable one (absent).
  const mayChange = canAct && (operator || !!limits?.user_changes_limits);
  const allowNoEnd = canAct && (operator || !!limits?.allow_no_end);
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

  const setWaitHours = async (hours: number) => {
    setBusy(true);
    try {
      // No canon string covers a capped WAIT (only ENDS_CAPPED, for the end) —
      // the re-rendered button already shows the actual value the server
      // applied, so a toast here would either invent copy or repeat that
      // number in seconds (the defect this replaced, F: "Capped at N sec").
      await runsApi.setRunEndAndWait(run.id, { waitBudgetSec: Math.round(hours * 3600) });
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
          canAct && (
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
          )
        ) : (
          mayChange && (
            <Button variant="ghost" size="sm" className="h-6 px-2 text-xs" onClick={() => setChangeOpen(true)}>
              {RL.ENDS_SET_AN_END}
            </Button>
          )
        )}
        {mayChange && run.ends_at && (
          <Button variant="ghost" size="sm" className="h-6 px-2 text-xs" onClick={() => setChangeOpen(true)}>
            {RL.ENDS_CHANGE}
          </Button>
        )}
        {/* M2 (packet:246): the "No end" hint is a fact about the STATE, shown
            regardless of who can change it — the mock's own "No end · where
            allowed" column pairs it with "No end" itself, not with the gate. */}
        {!run.ends_at && <span className="text-muted-foreground">{RL.NO_END_HINT}</span>}
        {!mayChange && run.ends_at && maxDays && (
          <span className="text-muted-foreground">{RL.endsLocked(maxDays)}</span>
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

      <ChangeEndDialog
        open={changeOpen}
        onOpenChange={setChangeOpen}
        run={run}
        onChanged={onChanged}
        maxDays={maxDays}
        allowNoEnd={allowNoEnd}
      />
    </div>
  );
}
