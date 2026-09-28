/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the run page's ONE lifetime banner (long-holds
// design rev 4 §2.3/§3/§4.1, mock "Before and at the end" / "Paused, and
// lost then revived"). Exactly one of Lost, Ended, Paused or the 24h/1h/10m
// warning renders at a time — they describe mutually exclusive server facts
// (lost_reason, paused_at, the ends_at countdown), never a client-side mode
// switch. A separate small note for end_tightened_at can sit alongside any
// of them: it is a "why did this change" fact, not a run situation.
import * as React from "react";
import { toast } from "sonner";
import type { ApprovalRequest, RunDetail } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import * as RL from "../../wardyn/copy/run-lifetime";
import { weekdayClock } from "./run-ends-row";

// Same three thresholds the board row's own chip warns at (runs-model.ts).
function endsWarningStageNow(run: RunDetail): RL.EndsWarningStage | null {
  const approxLeaseMs = run.ends_at ? Date.parse(run.ends_at) - Date.parse(run.created_at) : 0;
  return RL.endsWarningStage(run.ends_at, Date.now(), approxLeaseMs);
}

// Claude Code continues its conversation on revive; every other harness
// starts a new session with just its files back (design.md §4, LOST_BODY_*).
function isClaudeCode(agent: string): boolean {
  return agent === "claude-code" || agent === "claude_code";
}

export function RunLifetimeBanner({
  run,
  pending,
  onChanged,
}: {
  run: RunDetail;
  pending: ApprovalRequest[];
  onChanged: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [dismissed, setDismissed] = React.useState(false);

  const revive = async () => {
    setBusy(true);
    try {
      const res = await runsApi.reviveRun(run.id);
      if (res.denied_added.length > 0) toast.info(RL.revivePolicy(res.denied_added.length));
      onChanged();
    } catch (err) {
      toast.error("Couldn't revive this run", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const extend = async (ms: number) => {
    setBusy(true);
    try {
      const base = run.ends_at && Date.parse(run.ends_at) > Date.now() ? Date.parse(run.ends_at) : Date.now();
      await runsApi.setRunEndAndWait(run.id, { endsAt: new Date(base + ms).toISOString() });
      onChanged();
    } catch (err) {
      toast.error("Couldn't extend this run", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  // "Extend and revive" (mock, Ended state): a lease-ended run needs a new,
  // future end before a revive request can succeed — extendRefusal would
  // otherwise 400 on an end still in the past. Uses the run's own captured
  // default when it has one, falling back to a day (the same span the
  // warning banner's own "Extend 1 day" always offers).
  const extendAndRevive = async () => {
    setBusy(true);
    try {
      const defaultMs = (run.run_limits?.default_end_sec ?? 24 * 60 * 60) * 1000;
      await runsApi.setRunEndAndWait(run.id, { endsAt: new Date(Date.now() + defaultMs).toISOString() });
      const res = await runsApi.reviveRun(run.id);
      if (res.denied_added.length > 0) toast.info(RL.revivePolicy(res.denied_added.length));
      onChanged();
    } catch (err) {
      toast.error("Couldn't extend and revive this run", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const endRun = async () => {
    setBusy(true);
    try {
      await runsApi.killRun(run.id);
      onChanged();
    } catch (err) {
      toast.error(`Failed to end ${run.id}`, { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const resume = async () => {
    setBusy(true);
    try {
      await runsApi.resumeRun(run.id);
      onChanged();
    } catch (err) {
      toast.error("Couldn't resume this run", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  // ---- Lost (reboot / outage): kept, no network, Revive as the owner ----
  if (run.lost_reason === "reboot" || run.lost_reason === "outage") {
    return (
      <Banner data-testid="run-lifetime-lost" tone="danger" title={RL.LOST_TITLE} body={run.ends_at ? RL.lostBody(weekdayClock(run.ends_at)) : undefined}>
        <p className="mt-1 text-xs text-muted-foreground">{isClaudeCode(run.agent) ? RL.LOST_BODY_CLAUDE : RL.LOST_BODY_OTHER}</p>
        {run.lost_reason === "outage" && <p className="mt-1 text-xs text-muted-foreground">{RL.LOST_OUTAGE}</p>}
        <div className="mt-2.5 flex flex-wrap gap-2">
          <Button size="sm" onClick={() => void revive()} disabled={busy}>
            {busy ? RL.REVIVING : RL.REVIVE}
          </Button>
          <Button size="sm" variant="outline" onClick={() => void extend(24 * 60 * 60 * 1000)} disabled={busy}>
            {RL.ENDS_EXTEND}
          </Button>
          <Button size="sm" variant="outline" onClick={() => void endRun()} disabled={busy}>
            {RL.END_RUN}
          </Button>
        </div>
      </Banner>
    );
  }

  // ---- Ended: the lease ran out; kept for the grace, Extend + revive or End ----
  if (run.lost_reason === "ended") {
    return (
      <Banner data-testid="run-lifetime-ended" tone="plain" title={RL.ENDED_TITLE} body={run.lost_at ? RL.endedBody(weekdayClock(run.lost_at)) : undefined}>
        <div className="mt-2.5 flex flex-wrap gap-2">
          <Button size="sm" onClick={() => void extendAndRevive()} disabled={busy}>
            {RL.ENDED_EXTEND_AND_REVIVE}
          </Button>
          <Button size="sm" variant="outline" onClick={() => void endRun()} disabled={busy}>
            {RL.END_RUN}
          </Button>
        </div>
      </Banner>
    );
  }

  // ---- Paused: frozen because nobody is there, or on an open request ----
  if (run.paused_at) {
    if (run.paused_reason === "waiting") {
      // ApprovalRequest.expires_at (#567): min(requested_at + wait, ends_at),
      // computed server-side from the run row — the earliest one open is the
      // moment this pause itself gives up.
      const until = pending
        .map((p) => p.expires_at)
        .filter((v): v is string => !!v)
        .sort()[0];
      return (
        <Banner data-testid="run-lifetime-paused" tone="plain" title={RL.PAUSED_WAITING_TITLE} body={until ? RL.pausedWaitingBody(weekdayClock(until)) : undefined}>
          <div className="mt-2.5">
            <Button size="sm" onClick={() => void resume()} disabled={busy}>
              {RL.RESUME_NOW}
            </Button>
          </div>
        </Banner>
      );
    }
    const minutes = run.run_limits?.pause_idle_after_sec ? Math.round(run.run_limits.pause_idle_after_sec / 60) : 0;
    return (
      <Banner data-testid="run-lifetime-paused" tone="plain" title={RL.pausedIdleTitle(minutes)} body={RL.PAUSED_IDLE_BODY}>
        <div className="mt-2.5">
          <Button size="sm" onClick={() => void resume()} disabled={busy}>
            {RL.RESUME_NOW}
          </Button>
        </div>
      </Banner>
    );
  }

  // ---- Ending soon: the 24h/1h/10m warning (design.md §2.3) ----
  const stage = endsWarningStageNow(run);
  if (stage && !dismissed) {
    return (
      <Banner data-testid="run-lifetime-warning" tone="warn" title={RL.warnTitle(stage)} body={run.ends_at ? RL.warnBody(weekdayClock(run.ends_at)) : undefined}>
        <div className="mt-2.5 flex flex-wrap gap-2">
          <Button size="sm" onClick={() => void extend(24 * 60 * 60 * 1000)} disabled={busy}>
            {RL.WARN_EXTEND_1_DAY}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setDismissed(true)}>
            {RL.WARN_DISMISS}
          </Button>
        </div>
      </Banner>
    );
  }

  // ---- A tightened profile moved this run's end (RL-8, #573) ----
  if (run.end_tightened_at && run.ends_at) {
    return (
      <p data-testid="run-lifetime-tightened" className="mx-4 mt-2 shrink-0 rounded-md border border-warning/30 bg-warning-subtle px-2.5 py-2 text-xs text-warning">
        {RL.endsTightened(weekdayClock(run.ends_at))}
      </p>
    );
  }

  return null;
}

function Banner({
  tone,
  title,
  body,
  children,
  "data-testid": testId,
}: {
  tone: "warn" | "danger" | "plain";
  title: string;
  body?: string;
  children?: React.ReactNode;
  "data-testid": string;
}) {
  const toneClass =
    tone === "warn"
      ? "border-warning/40 bg-warning-subtle"
      : tone === "danger"
        ? "border-danger/40 bg-danger-subtle"
        : "border-border-strong bg-surface-2";
  return (
    <div data-testid={testId} className={`mx-4 mt-2 shrink-0 rounded-md border px-3 py-2.5 text-xs ${toneClass}`}>
      <h3 className="text-sm font-medium text-foreground">{title}</h3>
      {body && <p className="mt-1 text-muted-foreground">{body}</p>}
      {children}
    </div>
  );
}
