/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the run's lifetime controls (long-holds design rev 4 §2.3, mock "Ends": the four
// states), drawn since #3034 as two pill menus in the header's facts row, "Ends: never" and "Waits up to 24 h",
// with the explanations in the menu items; the full-width band they replaced is gone. Extending within the
// run's captured max is always allowed (it IS the lease); shortening,
// picking a date, No end, and the wait need the captured user_changes_limits
// gate — this component reads that gate off run.run_limits, never re-derives
// it, and PATCH /runs/{id} (runs.ts's setRunEndAndWait) is the one source of
// truth for whether an ask actually lands: a capped or refused response is
// rendered from what the server sent back, not guessed client-side.
//
// Self-contained (the LaunchWarningsNote/TerminalPane pattern): owns its own
// PATCH calls and dialog state, so the header needs only the one mount line.
import * as React from "react";
import { toast } from "sonner";
import type { RunDetail } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage, shortDate, shortTime, weekday } from "../../../lib/format";
import { useOperator, usePrincipal } from "../../wardyn/operator-context";
import { Check, ChevronDown } from "lucide-react";
import { Button } from "../../ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import { cn } from "../../ui/utils";
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
  return `${weekday(iso)} ${shortTime(iso)}`;
}

// How the Ended banner names kept_until. The default grace is exactly 7 days, so
// weekday + clock alone repeats the moment the run ended and reads as already
// past. By whole days away: under 6 is weekday + clock, 6 to 13 adds the
// packet's " next week", and 14 or more is a full local date with the clock.
export function keptUntilText(iso: string, now: number = Date.now()): string {
  const days = Math.round((Date.parse(iso) - now) / DAY_MS);
  if (days < 6) return weekdayClock(iso);
  if (days < 14) return `${weekdayClock(iso)} next week`;
  return `${shortDate(iso)} ${shortTime(iso)}`;
}

const PILL = "h-6 gap-1 rounded-full px-2.5 text-xs font-normal";
const ITEM = "flex-col items-start gap-0.5";
const LOCKED = "opacity-70";

/** The sentence a locked control's menu opens with: why it can't be changed. */
function Why({ text }: { text: string }) {
  return (
    <DropdownMenuLabel data-testid="lock-reason" className="max-w-[28ch] whitespace-normal text-meta font-normal text-muted-foreground">
      {text}
    </DropdownMenuLabel>
  );
}

/** A menu item with its explanation under it. */
function Item({ label, hint, current, onSelect }: { label: string; hint?: string; current?: boolean; onSelect?: () => void }) {
  return (
    <DropdownMenuItem disabled={current} onSelect={onSelect} className={cn(ITEM, current && "opacity-100")}>
      <span className="flex items-center gap-1.5">
        {label}
        {current && <Check className="size-3.5" aria-hidden />}
      </span>
      {hint && <span className="max-w-[28ch] whitespace-normal text-meta text-muted-foreground">{hint}</span>}
    </DropdownMenuItem>
  );
}

export function RunEndsControls({ run, onChanged }: { run: RunDetail; onChanged: () => void }) {
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

  // These are the LIVE-run controls; a lease-ended or otherwise lost run
  // (kept, no network) and every terminal state get the lifetime banner's own
  // Ended/Lost treatment instead (run-lifetime-banner.tsx) — showing both
  // would tell two different stories about the same run.
  // A QUEUED run has no end yet because it has not started (its start writes one), so "No end" would be
  // false and "Set an end" is refused; the controls wait for the start.
  if (isTerminalRunState(run.state) || run.lost_at || run.state === "QUEUED") return null;

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
  // #2112: a run with a lifetime cap is never given No end (run_end_wait.go planEnd refuses it for an operator
  // too), so the choice is not offered rather than refused after the click.
  const allowNoEnd = canAct && !run.lifetime_cap_at && (operator || !!limits?.allow_no_end);
  // Locked by an admin: the viewer may act on the run, but the run's captured limits keep its end and wait fixed.
  const locked = canAct && !mayChange;
  const maxDays = limits?.max_end_ahead_sec ? Math.ceil(limits.max_end_ahead_sec / (24 * 3600)) : undefined;

  const applyEndsAt = async (endsAt: string | null) => {
    setBusy(true);
    try {
      const res = await runsApi.setRunEndAndWait(run.id, { endsAt });
      if (res.capped.includes("ends_at") && res.ends_cap_loosened) {
        toast.warning(RL.ENDS_CAPPED_LOOSENED);
      } else if (res.capped.includes("ends_at") && res.latest_end) {
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
      // the re-rendered pill already shows the actual value the server
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

  const pill = (label: string) => (
    <>
      {label}
      <ChevronDown className="size-3" aria-hidden />
    </>
  );
  const wait = run.wait_budget_sec;

  return (
    <>
      {run.ends_at ? (
        canAct ? (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button data-testid="run-ends-pill" variant="outline" size="sm" className={cn(PILL, locked && LOCKED)} disabled={busy}>
                {pill(RL.endsPill(weekdayClock(run.ends_at)))}
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {/* R6-Run-Sheet: a locked run keeps its controls, greyed, and the menu says why. */}
              {locked && maxDays && <Why text={RL.endsLocked(maxDays)} />}
              <Item label={RL.ENDS_EXTEND_1_DAY} onSelect={() => extendBy(DAY_MS)} />
              <Item label={RL.ENDS_EXTEND_1_WEEK} onSelect={() => extendBy(WEEK_MS)} />
              {/* F12 (PR #1317 round-2 review): with no captured cap, "as
                  far as allowed" has no ceiling to name — 1 day/1 week already cover
                  the uncapped case, and Change… reaches any further date. */}
              {maxDays && <Item label={RL.endsExtendAsFarAsAllowed(maxDays)} onSelect={() => extendBy(FAR_FUTURE_MS)} />}
              {mayChange && <Item label={RL.ENDS_CHANGE} onSelect={() => setChangeOpen(true)} />}
            </DropdownMenuContent>
          </DropdownMenu>
        ) : (
          <span data-testid="run-ends-pill" className={cn("inline-flex items-center rounded-full border border-border text-muted-foreground", PILL)}>
            {RL.endsPill(weekdayClock(run.ends_at))}
          </span>
        )
      ) : (
        // M2 (packet:246): the "No end" state's own hint is a fact about the STATE, shown regardless of who can
        // change it — so the menu always opens, with No end checked, and Set an end… only where it is allowed.
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button data-testid="run-ends-pill" variant="outline" size="sm" className={cn(PILL, locked && LOCKED)} disabled={busy}>
              {pill(RL.ENDS_PILL_NEVER)}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            <Item label={RL.ENDS_NONE} hint={RL.NO_END_HINT} current />
            {mayChange && <Item label={RL.ENDS_SET_AN_END} hint={maxDays ? RL.endsHint(maxDays) : undefined} onSelect={() => setChangeOpen(true)} />}
          </DropdownMenuContent>
        </DropdownMenu>
      )}

      {typeof wait === "number" &&
        (canAct ? (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button data-testid="run-waits-pill" variant="outline" size="sm" className={cn(PILL, locked && LOCKED)} disabled={busy}>
                {pill(RL.waitsPill(wait))}
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {locked ? (
                <Why text={RL.waitLocked(wait)} />
              ) : (
                <>
                  <DropdownMenuLabel className="text-meta font-normal text-muted-foreground">{RL.WAIT_LABEL}</DropdownMenuLabel>
                  {[1, 4, 8, 24].map((h) => (
                    <Item key={h} label={RL.waitValueHours(h)} current={wait === h * 3600} onSelect={() => void setWaitHours(h)} />
                  ))}
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        ) : (
          <span data-testid="run-waits-pill" className={cn("inline-flex items-center rounded-full border border-border text-muted-foreground", PILL)}>
            {RL.waitsPill(wait)}
          </span>
        ))}

      <ChangeEndDialog
        open={changeOpen}
        onOpenChange={setChangeOpen}
        run={run}
        onChanged={onChanged}
        maxDays={maxDays}
        allowNoEnd={allowNoEnd}
      />
    </>
  );
}
