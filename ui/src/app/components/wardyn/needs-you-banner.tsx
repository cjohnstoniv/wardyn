/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Needs you" on the run (#2963, mock M8 Option A): the agent's request as a banner, the takeover
// strip while the person has control, and the one-click hand back with its confirmation. The three
// verbs are internal/api/approvals_takeover.go's; the server is the gate (the attach entry rule), so
// this only hides buttons from someone who could not use them.
import * as React from "react";
import { ChevronDown } from "lucide-react";
import type { ApprovalRequest, RunDetail } from "../../lib/types";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { health as healthApi } from "../../lib/api/health";
import { getErrorMessage } from "../../lib/format";
import { mayEnterRunOrUnknown } from "../../lib/run-entry";
import { Button } from "../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { enterUIApp } from "../screens/run-detail-ssh";
import { HARNESS_LOGIN_TASK } from "../screens/run-detail/sign-in-run";
import { useOperator, useOperatorResolved, usePrincipal } from "./operator-context";
import { NEEDS_YOU, askedAgo, heldClock, type TakeoverSurface } from "./copy/needs-you";
import { toast } from "sonner";

/** The run's open takeover request: the newest human_takeover still waiting or still held. */
export function activeTakeover(approvals: ApprovalRequest[]): ApprovalRequest | undefined {
  return approvals
    .filter((a) => a.kind === "human_takeover" && (a.state === "PENDING" || (a.state === "APPROVED" && !a.handed_back_at)))
    .sort((a, b) => Date.parse(b.requested_at) - Date.parse(a.requested_at))[0];
}

/** Where a takeover of run puts the person, as the run page can tell: its declared desktop, else its terminal. */
export function surfaceOf(run: RunDetail): TakeoverSurface {
  return (run.ui_apps ?? []).some((a) => a.name === "novnc") ? "desktop" : "terminal";
}

const snoozeKey = (id: string) => `wardyn.needs-you.snooze.${id}`;

function readSnooze(id: string): number {
  try {
    return Number(window.localStorage.getItem(snoozeKey(id)) ?? 0) || 0;
  } catch {
    return 0;
  }
}

function writeSnooze(id: string, until: number) {
  try {
    window.localStorage.setItem(snoozeKey(id), String(until));
  } catch {
    // Snooze is a per-browser convenience; without storage it lasts until the page reloads.
  }
}

const clockTime = (d: Date) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

export function NeedsYouBanner({
  run,
  approvals,
  onChanged,
  onOpenTerminal,
  onGoAudit,
}: {
  run: RunDetail;
  approvals: ApprovalRequest[];
  onChanged: () => void;
  onOpenTerminal: () => void;
  onGoAudit: () => void;
}) {
  const principal = usePrincipal();
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const ap = activeTakeover(approvals);
  const apId = ap?.id;
  const [busy, setBusy] = React.useState<"take" | "back" | "decline" | null>(null);
  const [done, setDone] = React.useState(false);
  const [now, setNow] = React.useState(() => Date.now());
  const [snoozedUntil, setSnoozedUntil] = React.useState(0);
  React.useEffect(() => setSnoozedUntil(apId ? readSnooze(apId) : 0), [apId]);
  React.useEffect(() => {
    if (!apId) return;
    const t = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(t);
  }, [apId]);

  // Mirrors the server's takeoverRecorded: a runner that declined still has the console and SSH
  // sessions recorded here; an organisation that keeps no recordings, or a harness login run (never
  // recorded), means the takeover is not recorded.
  const recorded = run.not_recorded !== "org_off" && run.task !== HARNESS_LOGIN_TASK;

  if (done) {
    return (
      <section data-testid="needs-you-done" className="mx-4 mt-3 rounded-lg border border-success/40 bg-success-subtle px-4 py-3 text-sm">
        <p className="font-semibold text-foreground">{NEEDS_YOU.done.title}</p>
        <p className="text-muted-foreground">{NEEDS_YOU.done.working}</p>
        <div className="mt-2 flex gap-2">
          <Button size="sm" variant="outline" onClick={onGoAudit}>
            {NEEDS_YOU.done.audit}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setDone(false)}>
            {NEEDS_YOU.done.dismiss}
          </Button>
        </div>
      </section>
    );
  }
  if (!ap) return null;

  const mayEnter = mayEnterRunOrUnknown(run, principal, operator, resolved);
  const reason = String(ap.requested_scope.reason ?? "");
  const surface = surfaceOf(run);

  const takeOver = async () => {
    setBusy("take");
    try {
      const got = await approvalsApi.takeOver(ap.id);
      if (got.surface === "desktop") {
        const h = await healthApi.health();
        await enterUIApp(run.id, "novnc", h.ui_sandbox ?? null);
      } else {
        onOpenTerminal();
      }
      onChanged();
    } catch (err) {
      toast.error(NEEDS_YOU.failed.takeOver, { description: getErrorMessage(err) });
    } finally {
      setBusy(null);
    }
  };
  const handBack = async () => {
    setBusy("back");
    try {
      await approvalsApi.handBack(ap.id);
      setDone(true);
      onChanged();
    } catch (err) {
      toast.error(NEEDS_YOU.failed.handBack, { description: getErrorMessage(err) });
    } finally {
      setBusy(null);
    }
  };
  const decline = async () => {
    setBusy("decline");
    try {
      await approvalsApi.decline(ap.id);
      onChanged();
    } catch (err) {
      toast.error(NEEDS_YOU.failed.decline, { description: getErrorMessage(err) });
    } finally {
      setBusy(null);
    }
  };
  const snooze = (ms: number) => {
    const until = Date.now() + ms;
    writeSnooze(ap.id, until);
    setSnoozedUntil(until);
  };

  if (ap.state === "APPROVED" && ap.takeover_holder === principal) {
    const since = Date.parse(ap.decided_at ?? ap.requested_at);
    return (
      <section data-testid="needs-you-control" className="mx-4 mt-3 flex flex-wrap items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 px-4 py-2.5 text-sm">
        <span className="font-semibold text-foreground">{NEEDS_YOU.strip.control(surface)}</span>
        {/* No recorder pauses for a takeover yet, so a recorded run's strip says nothing about it. */}
        {!recorded && (
          <span data-testid="needs-you-recording" className="text-muted-foreground" title={NEEDS_YOU.strip.offDetail}>
            {NEEDS_YOU.strip.off}
          </span>
        )}
        <span data-testid="needs-you-clock" className="font-mono text-muted-foreground">{heldClock(now - since)}</span>
        <Button size="sm" className="ml-auto" disabled={busy !== null} onClick={() => void handBack()}>
          {busy === "back" ? NEEDS_YOU.strip.handingBack : NEEDS_YOU.strip.handBack}
        </Button>
      </section>
    );
  }

  const actions = mayEnter && (
    <div className="flex shrink-0 items-center gap-2">
      <Button size="sm" disabled={busy !== null} onClick={() => void takeOver()}>
        {NEEDS_YOU.banner.takeOver}
      </Button>
      {ap.state === "PENDING" && (
        <>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="sm" variant="outline" disabled={busy !== null}>
                {NEEDS_YOU.banner.snooze} <ChevronDown className="size-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => snooze(10 * 60_000)}>{NEEDS_YOU.banner.snooze10}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => snooze(60 * 60_000)}>{NEEDS_YOU.banner.snooze60}</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
          <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => void decline()}>
            {NEEDS_YOU.banner.decline}
          </Button>
        </>
      )}
    </div>
  );

  if (ap.state === "APPROVED") {
    const who = ap.takeover_holder ?? "";
    return (
      <section data-testid="needs-you-in-use" className="mx-4 mt-3 flex flex-wrap items-center gap-3 rounded-lg border border-warning/40 bg-warning-subtle px-4 py-2.5 text-sm">
        <div className="min-w-0 flex-1">
          <p className="font-semibold text-foreground">{NEEDS_YOU.inUse.title(who, clockTime(new Date(ap.decided_at ?? ap.requested_at)))}</p>
          <p className="text-muted-foreground">{NEEDS_YOU.inUse.detail(who, surface)}</p>
        </div>
        {actions}
      </section>
    );
  }

  if (snoozedUntil > now) {
    return (
      <section data-testid="needs-you-snoozed" className="mx-4 mt-3 flex items-center gap-3 rounded-lg border border-warning/30 px-4 py-1.5 text-sm">
        <span className="text-muted-foreground">{NEEDS_YOU.banner.snoozedUntil(clockTime(new Date(snoozedUntil)))}</span>
        {mayEnter && (
          <Button size="sm" variant="outline" className="ml-auto" disabled={busy !== null} onClick={() => void takeOver()}>
            {NEEDS_YOU.banner.takeOver}
          </Button>
        )}
      </section>
    );
  }

  return (
    <section
      data-testid="needs-you-banner"
      className="mx-4 mt-3 flex flex-wrap items-start gap-3 rounded-lg border border-warning/40 bg-warning-subtle px-4 py-3 text-sm"
    >
      <div className="min-w-0 flex-1">
        <p className="font-semibold text-foreground">{NEEDS_YOU.banner.title}</p>
        <blockquote data-testid="needs-you-reason" className="mt-1 text-foreground">
          {reason}
        </blockquote>
        <p className="mt-1 text-muted-foreground">
          {mayEnter ? NEEDS_YOU.banner.asked(askedAgo(now - Date.parse(ap.requested_at)), surface) : NEEDS_YOU.readOnly(run.created_by)}
        </p>
      </div>
      {actions}
    </section>
  );
}
