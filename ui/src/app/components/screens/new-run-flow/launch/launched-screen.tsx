/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Launched: where a create lands, at ?launched=<id>. It reads the run by its id,
// so a reload shows the same screen and the draft is not involved. Starting shows
// an indeterminate bar; Waiting shows the place in line, since when and when the
// run gives up, and the run can be cancelled through the kill dialog.

import * as React from "react";
import { Link, useLocation } from "react-router-dom";
import { runs } from "../../../../lib/api/runs";
import { getErrorMessage } from "../../../../lib/format";
import { runHeadline } from "../../../../lib/types/runs";
import { Button, buttonVariants } from "../../../ui/button";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { KillRunDialog } from "../../../wardyn/kill-run-dialog";
import { Chip, RunStateBadge } from "../../../wardyn/primitives";
import { ErrorState } from "../../../wardyn/states";
import { useRequestIdentity } from "../../../wardyn/operator-context";
import { useNewRunDispatch, useNewRunDraft, type LaunchedState, type NewRunScreenProps } from "../new-run-route";
import { givesUp, placeInLine, waitingBody, waitingSince, type LaunchedRun } from "./launched-model";
import { useLaunchedRun } from "./use-launched-run";

/** The create this person just made for this run, or none: another person's, or another run's, is never shown. */
function createdFor(state: unknown, principal: string, runId: string): LaunchedRun | undefined {
  const s = state as Partial<LaunchedState> | null;
  return s?.created?.id === runId && s.owner === principal ? (s.created as LaunchedRun) : undefined;
}

export function LaunchedScreen({ location, go }: NewRunScreenProps) {
  return location.screen === "launched" ? <Launched runId={location.runId} startAnother={() => go({ screen: "start" })} /> : null;
}

function Launched({ runId, startAnother }: { runId: string; startAnother: () => void }) {
  const { principal } = useRequestIdentity();
  const router = useLocation();
  const created = React.useMemo(() => createdFor(router.state, principal, runId), [router.state, principal, runId]);
  const { run, failed, now, reload, pool } = useLaunchedRun(runId, created);
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const heading = React.useRef<HTMLHeadingElement>(null);
  const [killing, setKilling] = React.useState<string | null>(null);
  const [killFailed, setKillFailed] = React.useState("");

  // The create is done with the draft: what was typed does not come back.
  const owner = draft.owner;
  React.useEffect(() => {
    dispatch({ type: "reset", owner });
  }, [dispatch, owner]);

  const loaded = run?.id === runId;
  const waiting = run?.state === "QUEUED";
  const starting = run !== null && run !== undefined && ["PENDING", "STARTING"].includes(run.state as string);
  const badge = waiting ? NR_LAUNCH.BADGE_WAITING : starting ? NR_LAUNCH.BADGE_STARTING : undefined;
  React.useEffect(() => {
    if (!loaded) return;
    heading.current?.focus();
  }, [loaded]);
  // Cancel run leaves with the waiting state; focus goes to the heading rather than to the body.
  const wasWaiting = React.useRef(false);
  React.useEffect(() => {
    if (wasWaiting.current && !waiting) heading.current?.focus();
    wasWaiting.current = waiting;
  }, [waiting]);

  if (run === null || (failed && !loaded)) {
    return <ErrorState as="h1" onRetry={failed ? reload : undefined} />;
  }
  if (!loaded) return null;

  const queue = run.queue;
  const body = waiting ? queue && waitingBody(queue.reason, pool) : starting ? NR_LAUNCH.STARTING_BODY : undefined;
  const kill = async (id: string) => {
    setKilling(null);
    setKillFailed("");
    try {
      await runs.killRun(id);
      reload();
    } catch (e) {
      setKillFailed(getErrorMessage(e));
    }
  };

  return (
    <section className="mx-auto flex w-full max-w-xl flex-col items-center gap-4 text-center">
      {badge ? <Chip tone={waiting ? "info" : "success"}>{badge}</Chip> : <RunStateBadge state={run.state} />}
      <h1 ref={heading} tabIndex={-1} className="font-semibold tracking-tight outline-none">{runHeadline(run)}</h1>
      {body && <p className="text-muted-foreground">{body}</p>}
      {starting && (
        <div role="progressbar" aria-label={NR_LAUNCH.BADGE_STARTING} className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
          <div className="h-full w-full animate-pulse rounded-full bg-primary motion-reduce:animate-none" />
        </div>
      )}
      {waiting && queue && (
        <dl className="grid w-full gap-3 rounded-lg border border-border p-4 text-left sm:grid-cols-3">
          <div>
            <dt className="text-sm text-muted-foreground">{NR_LAUNCH.POSITION}</dt>
            <dd className="font-medium">{placeInLine(queue)}</dd>
          </div>
          <div>
            <dt className="text-sm text-muted-foreground">{NR_LAUNCH.WAITING_SINCE}</dt>
            <dd className="font-medium">{waitingSince(queue.queued_at, now)}</dd>
          </div>
          <div>
            <dt className="text-sm text-muted-foreground">{NR_LAUNCH.GIVES_UP}</dt>
            <dd className="font-medium">{givesUp(queue.deadline_at, now)}</dd>
          </div>
        </dl>
      )}
      {waiting && (
        <Button type="button" variant="destructive" onClick={() => setKilling(run.id)}>
          {NR_LAUNCH.CANCEL_RUN}
        </Button>
      )}
      {killFailed && (
        <p role="alert" className="text-sm text-danger">
          {killFailed}
        </p>
      )}
      <div className="flex flex-wrap justify-center gap-2">
        <Button type="button" variant="outline" onClick={startAnother}>
          {NR_LAUNCH.START_ANOTHER}
        </Button>
        <Link to={`/runs/${encodeURIComponent(run.id)}`} state={{ launchWarnings: created?.warnings ?? [] }} className={buttonVariants()}>
          {NR_LAUNCH.VIEW_RUN}
        </Link>
      </div>
      <KillRunDialog runId={killing} onOpenChange={(open) => !open && setKilling(null)} onConfirm={(id) => void kill(id)} />
    </section>
  );
}
