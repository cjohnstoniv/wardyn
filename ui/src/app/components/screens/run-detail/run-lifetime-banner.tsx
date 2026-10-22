/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — the run page's ONE lifetime banner (long-holds
// design rev 4 §2.3/§3/§4.1, mock "Before and at the end" / "Paused, and
// lost then revived"). Exactly one of Lost, Ended, Paused or the 24h/1h/10m
// warning renders at a time — they describe mutually exclusive server facts
// (lost_reason, paused_at, the ends_at countdown), never a client-side mode
// switch. The tightened note (F19, PR #1317 review) is a separate "why did
// this change" fact and DOES render alongside whichever of those is primary —
// see renderPrimary below, which the terminal guard and the tightened note
// both wrap, rather than joining the same if/else chain.
import * as React from "react";
import { toast } from "sonner";
import type { ApprovalRequest, RunDetail } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { audit as auditApi } from "../../../lib/api/audit";
import { runs as runsApi } from "../../../lib/api/runs";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import { useOperator, usePrincipal } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import * as RL from "../../wardyn/copy/run-lifetime";
import { LOCAL_RUN, runnerLabel } from "../../wardyn/copy/runners";
import { keptUntilText, weekdayClock } from "./run-ends-row";
import { ChangeEndDialog } from "./change-end-dialog";
import { reviveWithToasts } from "../runs/revive-run";

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

// F8 (PR #1317 review): one shared `busy` boolean made EVERY button read
// "Reviving…" while any of the four actions below was in flight — clicking
// Extend on the Lost banner relabelled Revive too. Track WHICH action is
// running instead.
type Action = "revive" | "extend" | "endRun" | "resume" | null;

// Whether a local run lost to an outage or a reboot was stopped LATE: its runner came back with the network still on and
// the org had it stopped then (a run.containment.reassert row with containment "late"). Only that row tells the
// two stopped sentences apart, so it is read for a local outage-lost run alone.
//
// null means "not read yet" (or read for a different loss than this one): the banner stays silent. "failed" means the
// read was made and failed: the stop's timing is unknown, so the banner keeps its title and its actions but claims
// neither sentence. Only a read that succeeded may answer false, because false is what lets the banner say the stop
// was on time. A late row counts only
// when it names this loss (its lost_at is the run's current one), so a row left by an earlier loss of a revived run
// proves nothing about this one.
function useStoppedLate(run: RunDetail): boolean | "failed" | null {
  const applies = run.placement === "local" && (run.lost_reason === "outage" || run.lost_reason === "reboot");
  const loss = `${run.id}@${run.lost_at ?? ""}`;
  const [read, setRead] = React.useState<{ loss: string; late: boolean | "failed" } | null>(null);
  React.useEffect(() => {
    if (!applies) return;
    let live = true;
    const lostAt = Date.parse(run.lost_at ?? "");
    auditApi
      .listAudit(run.id, { action: "run.containment.reassert" })
      .then(
        (rows) =>
          live &&
          setRead({
            loss,
            late: rows.some((r) => r.data?.containment === "late" && Date.parse(String(r.data?.lost_at)) === lostAt),
          }),
      )
      .catch(() => live && setRead({ loss, late: "failed" }));
    return () => {
      live = false;
    };
  }, [applies, run.id, run.lost_at, run.runner_online, loss]);
  return applies && read?.loss === loss ? read.late : null;
}

export function RunLifetimeBanner({
  run,
  pending,
  onChanged,
  containmentPending = false,
}: {
  run: RunDetail;
  pending: ApprovalRequest[];
  onChanged: () => void;
  /** The runner holds the run as containment pending: its proxy is not proven stopped, so no stopped sentence. */
  containmentPending?: boolean;
}) {
  const [action, setAction] = React.useState<Action>(null);
  const [dismissed, setDismissed] = React.useState(false);
  const [changeOpen, setChangeOpen] = React.useState(false);
  const operator = useOperator();
  const principal = usePrincipal();
  // F16 (PR #1317 review): every action here is owner-or-super-admin on the
  // server (run_revive.go, run_pause.go, run_end_wait.go) — a security admin
  // looking at a foreign run must not be offered a button that always 403s.
  const canAct = operator || run.created_by === principal;
  // R2-3 (PR #1317 round-2 review): the SAME formula run-ends-row.tsx's own
  // Change…/Set an end… gate uses — Change end… (and its dialog's No end
  // option) needs the captured user_changes_limits gate too, not just
  // ownership; the server refuses an earlier end or No end without it
  // (run_end_wait.go), regardless of who is asking.
  const limits = run.run_limits;
  const mayChange = canAct && (operator || !!limits?.user_changes_limits);
  const allowNoEnd = canAct && !run.lifetime_cap_at && (operator || !!limits?.allow_no_end);

  const revive = async () => {
    setAction("revive");
    try {
      if (await reviveWithToasts(run.id, "Couldn't revive this run")) onChanged();
    } finally {
      setAction(null);
    }
  };

  const extend = async (ms: number) => {
    setAction("extend");
    try {
      const base = run.ends_at && Date.parse(run.ends_at) > Date.now() ? Date.parse(run.ends_at) : Date.now();
      await runsApi.setRunEndAndWait(run.id, { endsAt: new Date(base + ms).toISOString() });
      onChanged();
    } catch (err) {
      toast.error("Couldn't extend this run", { description: getErrorMessage(err) });
    } finally {
      setAction(null);
    }
  };

  // "Extend and revive" (mock, Ended state): a lease-ended run needs a new,
  // future end before a revive request can succeed — extendRefusal would
  // otherwise 400 on an end still in the past. Uses the run's own captured
  // default when it has one, falling back to a day (the same span the
  // warning banner's own "Extend 1 day" always offers).
  const extendAndRevive = async () => {
    setAction("revive");
    try {
      const defaultMs = (run.run_limits?.default_end_sec ?? 24 * 60 * 60) * 1000;
      try {
        await runsApi.setRunEndAndWait(run.id, { endsAt: new Date(Date.now() + defaultMs).toISOString() });
      } catch (err) {
        toast.error("Couldn't extend and revive this run", { description: getErrorMessage(err) });
        return;
      }
      await reviveWithToasts(run.id, "Couldn't extend and revive this run");
      // The end has moved whatever the revive did, so the page re-reads either way.
      onChanged();
    } finally {
      setAction(null);
    }
  };

  const endRun = async () => {
    setAction("endRun");
    try {
      await runsApi.killRun(run.id);
      onChanged();
    } catch (err) {
      toast.error(`Failed to end ${run.id}`, { description: getErrorMessage(err) });
    } finally {
      setAction(null);
    }
  };

  const resume = async () => {
    setAction("resume");
    try {
      await runsApi.resumeRun(run.id);
      onChanged();
    } catch (err) {
      toast.error("Couldn't resume this run", { description: getErrorMessage(err) });
    } finally {
      setAction(null);
    }
  };

  const busy = action !== null;
  const stoppedLate = useStoppedLate(run);
  const local = run.placement === "local";
  const runner = runnerLabel(run);

  // renderPrimary — the Lost/Ended/Paused/warning priority chain, as its own
  // function (not a component-level early return) so the tightened note
  // below can still render alongside whichever of these wins (F19).
  function renderPrimary(): React.ReactNode {
    // ---- A local run its runner lost contact with, whatever the lost reason (RS4b/c/d, RN3-D6): what the runner did, as it reported it ----
    // The remote sentences say Wardyn was unreachable; for a computer that went offline that is false, and
    // whether the network was stopped is the runner's report, not Wardyn's own act. Chosen from server facts
    // only: the runner's liveness and the late-stop row, never elapsed time.
    if (local && (run.lost_reason === "outage" || run.lost_reason === "reboot")) {
      // Only a runner that is known to be connected can have confirmed the stop. One reported offline cannot, and one the
      // server cannot report on (WARDYN_HA: this replica may not hold its connection) is neither: it keeps Revive and
      // Extend, since they are the only way to act, and a revive the server cannot serve says why.
      const offline = run.runner_online === false && stoppedLate !== true;
      const unknown = run.runner_online === undefined && stoppedLate !== true;
      // Online and the late-stop row not read yet: say nothing rather than the wrong stopped sentence. A read that
      // failed is different: the title and Revive / Extend / End run stay (they are the only way to act on the run),
      // and only the on-time / late sentence is left out.
      if (!offline && !unknown && stoppedLate === null) return null;
      const seen = run.runner_last_seen_at ? absoluteTime(run.runner_last_seen_at) : "—";
      return (
        <Banner
          data-testid={unknown ? "run-lifetime-local-unknown" : offline ? "run-lifetime-local-lapse" : "run-lifetime-local-stopped"}
          tone="danger"
          title={unknown ? LOCAL_RUN.UNKNOWN_TITLE(runner) : offline ? LOCAL_RUN.LAPSE_TITLE(runner) : LOCAL_RUN.STOPPED_TITLE}
          body={
            unknown
              ? LOCAL_RUN.UNKNOWN_BODY(runner, seen)
              : offline
                ? LOCAL_RUN.LAPSE_BODY(runner, seen)
                : stoppedLate === "failed" || containmentPending
                  ? undefined
                  : stoppedLate
                    ? LOCAL_RUN.STOPPED_LATE(runner)
                    : LOCAL_RUN.STOPPED_BODY(runner)
          }
        >
          {offline && <p className="mt-1 text-xs text-muted-foreground">{LOCAL_RUN.REVIVE_WHEN_BACK(runner)}</p>}
          {canAct && (
            <div className="mt-2.5 flex flex-wrap gap-2">
              {!offline && (
                <>
                  <Button size="sm" onClick={() => void revive()} disabled={busy}>
                    {action === "revive" ? RL.REVIVING : RL.REVIVE}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => void extend(24 * 60 * 60 * 1000)} disabled={busy}>
                    {RL.ENDS_EXTEND}
                  </Button>
                </>
              )}
              <Button size="sm" variant="outline" onClick={() => void endRun()} disabled={busy}>
                {RL.END_RUN}
              </Button>
            </div>
          )}
        </Banner>
      );
    }

    // ---- Lost (reboot / outage): kept, no network, Revive as the owner ----
    if (run.lost_reason === "reboot" || run.lost_reason === "outage") {
      const outage = run.lost_reason === "outage";
      // M4 (PR #1317 review): no k8s revive in 0.8 (L6) — a node-restarted run
      // keeps only what a drive holds, and there is nothing here to revive.
      const k8s = run.runner_target === "k8s";
      return (
        <Banner data-testid="run-lifetime-lost" tone="danger" title={RL.LOST_TITLE} body={outage || k8s ? undefined : run.ends_at ? RL.lostBody(weekdayClock(run.ends_at)) : undefined}>
          {k8s ? (
            <p className="mt-1 text-xs text-muted-foreground">{RL.LOST_K8S}</p>
          ) : outage ? (
            // F5 (PR #1317 review): the design gives the outage its own single
            // sentence (LOST_OUTAGE) — nothing rebooted, so LOST_BODY's "the
            // machine it ran on restarted" and the harness-continuity line
            // (which answers a question about a restart that never happened)
            // both give way to it, rather than all three stacking as
            // contradictory sentences.
            <p className="mt-1 text-xs text-muted-foreground">{RL.LOST_OUTAGE}</p>
          ) : (
            <p className="mt-1 text-xs text-muted-foreground">{isClaudeCode(run.agent) ? RL.LOST_BODY_CLAUDE : RL.LOST_BODY_OTHER}</p>
          )}
          {canAct && (
            <div className="mt-2.5 flex flex-wrap gap-2">
              {/* F18 (PR #1317 round-2 review): no revive on k8s in 0.8 (L6) —
                  Revive/Extend stay Docker-only, but the owner can still End
                  the run either way. */}
              {!k8s && (
                <>
                  <Button size="sm" onClick={() => void revive()} disabled={busy}>
                    {action === "revive" ? RL.REVIVING : RL.REVIVE}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => void extend(24 * 60 * 60 * 1000)} disabled={busy}>
                    {RL.ENDS_EXTEND}
                  </Button>
                </>
              )}
              <Button size="sm" variant="outline" onClick={() => void endRun()} disabled={busy}>
                {RL.END_RUN}
              </Button>
            </div>
          )}
        </Banner>
      );
    }

    // ---- Ended: the lease ran out; kept for the grace, Extend + revive or End ----
    if (run.lost_reason === "ended") {
      // R2-2 (PR #1317 round-2 review, F9 rejected): a task (non-interactive)
      // run's agent ran once at dispatch — reviveEligible always refuses one
      // ("task run's agent cannot be started again", run_revive.go), and
      // run_end_wait.go is explicit that extending an already-ended run
      // changes nothing observable (no network, no revive, no extra file
      // time — the grace is counted from lost_at, not from a later PATCH).
      // Offering Extend here would be a button that does nothing; End run is
      // the only real action left.
      return (
        <Banner
          data-testid="run-lifetime-ended"
          tone="plain"
          title={RL.ENDED_TITLE}
          body={run.interactive ? (run.kept_until ? RL.endedBody(keptUntilText(run.kept_until)) : RL.ENDED_BODY_NO_DATE) : RL.ENDED_BODY_TASK}
        >
          {canAct && (
            <div className="mt-2.5 flex flex-wrap gap-2">
              {run.interactive && (
                <Button size="sm" onClick={() => void extendAndRevive()} disabled={busy}>
                  {RL.ENDED_EXTEND_AND_REVIVE}
                </Button>
              )}
              <Button size="sm" variant="outline" onClick={() => void endRun()} disabled={busy}>
                {RL.END_RUN}
              </Button>
            </div>
          )}
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
            {canAct && (
              <div className="mt-2.5">
                <Button size="sm" onClick={() => void resume()} disabled={busy}>
                  {RL.RESUME_NOW}
                </Button>
              </div>
            )}
          </Banner>
        );
      }
      // F14 (PR #1317 review): the server never pauses an idle run sooner than
      // pauseDelayFloor (630s), even under a profile that sets less — showing
      // the raw setting understated how long it actually takes.
      const minutes = Math.round(Math.max(run.run_limits?.pause_idle_after_sec ?? 0, RL.PAUSE_IDLE_FLOOR_SEC) / 60);
      return (
        <Banner data-testid="run-lifetime-paused" tone="plain" title={RL.pausedIdleTitle(minutes)} body={RL.PAUSED_IDLE_BODY}>
          {canAct && (
            <div className="mt-2.5">
              <Button size="sm" onClick={() => void resume()} disabled={busy}>
                {RL.RESUME_NOW}
              </Button>
            </div>
          )}
        </Banner>
      );
    }

    // ---- A local run on a computer that is offline (RS4a): the run goes on there ----
    // Only the runner's own liveness says so; nothing is inferred from time. No buttons: Kill stays in the
    // command bar, and nothing can be sent to the computer until it reconnects.
    if (local && run.state === "RUNNING" && run.runner_online === false) {
      return (
        <Banner
          data-testid="run-lifetime-local-offline"
          tone="warn"
          title={LOCAL_RUN.OFFLINE_TITLE(runner)}
          body={`${LOCAL_RUN.OFFLINE_BODY(runner, run.runner_last_seen_at ? absoluteTime(run.runner_last_seen_at) : "—")} ${LOCAL_RUN.OFFLINE_SPOOL}`}
        />
      );
    }

    // ---- Ending soon: the 24h/1h/10m warning (design.md §2.3) ----
    const stage = endsWarningStageNow(run);
    if (stage && !dismissed) {
      return (
        <Banner data-testid="run-lifetime-warning" tone="warn" title={RL.warnTitle(stage)} body={run.ends_at ? RL.warnBody(weekdayClock(run.ends_at)) : undefined}>
          <div className="mt-2.5 flex flex-wrap gap-2">
            {canAct && (
              <Button size="sm" onClick={() => void extend(24 * 60 * 60 * 1000)} disabled={busy}>
                {RL.WARN_EXTEND_1_DAY}
              </Button>
            )}
            {/* R2-3 (PR #1317 round-2 review): Change end… needs the SAME
                user_changes_limits gate run-ends-row.tsx's own Change… does —
                the server refuses an earlier end or No end without it
                (run_end_wait.go), so offering the dialog to a locked member
                would open one whose only live options always 403. */}
            {mayChange && (
              <Button size="sm" variant="outline" onClick={() => setChangeOpen(true)} disabled={busy}>
                {RL.WARN_CHANGE_END}
              </Button>
            )}
            <Button size="sm" variant="ghost" onClick={() => setDismissed(true)}>
              {RL.WARN_DISMISS}
            </Button>
          </div>
        </Banner>
      );
    }

    return null;
  }

  // F3 (PR #1317 review): the server never clears lost_*/paused_* on a
  // terminal transition (store.go, store_run_lease.go) — only a revive does.
  // Without this guard, a KILLED run that was lost or lease-ended kept
  // showing Revive/Extend/End run forever after End run, and every
  // lease-ended run the grace eventually stops did the same.
  if (isTerminalRunState(run.state)) return null;

  return (
    <>
      {renderPrimary()}
      {run.end_tightened_at && run.ends_at && (
        <p data-testid="run-lifetime-tightened" className="mx-4 mt-2 shrink-0 rounded-md border border-warning/30 bg-warning-subtle px-2.5 py-2 text-xs text-warning">
          {RL.endsTightened(weekdayClock(run.ends_at))}
        </p>
      )}
      <ChangeEndDialog
        open={changeOpen}
        onOpenChange={setChangeOpen}
        run={run}
        onChanged={onChanged}
        maxDays={limits?.max_end_ahead_sec ? Math.ceil(limits.max_end_ahead_sec / (24 * 3600)) : undefined}
        allowNoEnd={allowNoEnd}
      />
    </>
  );
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
