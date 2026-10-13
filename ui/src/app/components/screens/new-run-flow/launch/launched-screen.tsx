/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Launched: where a create lands, at ?launched=<id>. It reads the run by its id,
// so a reload shows the same screen and the draft is not involved. Starting shows
// an indeterminate bar; Waiting shows the place in line, since when and when the
// run gives up, and Cancel run cancels it at once.

import * as React from "react";
import { Check } from "lucide-react";
import { Link, useLocation } from "react-router-dom";
import { runs } from "../../../../lib/api/runs";
import { getErrorMessage } from "../../../../lib/format";
import { runHeadline } from "../../../../lib/types/runs";
import { Button, buttonVariants } from "../../../ui/button";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
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
    setKillFailed("");
    try {
      await runs.killRun(id);
      reload();
    } catch (e) {
      setKillFailed(getErrorMessage(e));
    }
  };

  return (
    <section className="flex flex-col items-center gap-4 text-center">
      {badge && <span
        aria-hidden="true"
        className={waiting
          ? "grid size-[54px] place-items-center rounded-full border border-info/40 bg-info-subtle text-[22px] text-info"
          : "grid size-[54px] place-items-center rounded-full border border-success/40 bg-success-subtle text-success"}
      >
        {waiting ? "…" : <Check className="size-[26px]" />}
      </span>}
      {badge ? <Chip tone={waiting ? "info" : "success"}>{badge}</Chip> : <RunStateBadge state={run.state} />}
      <h1 ref={heading} tabIndex={-1} className="font-semibold tracking-tight outline-none">{runHeadline(run)}</h1>
      {body && <p className="text-muted-foreground">{body}</p>}
      {starting && (
        <div role="progressbar" aria-label={NR_LAUNCH.BADGE_STARTING} className="my-4 h-1.5 w-64 overflow-hidden rounded-full bg-muted">
          <div className="h-full w-full animate-pulse rounded-full bg-primary motion-reduce:animate-none" />
        </div>
      )}
      {waiting && (
        <div className="my-4 grid w-full gap-3.5 rounded-lg border border-border bg-card p-4 text-left sm:grid-cols-3">
          {queue && (
            <dl className="contents">
              <div>
                <dt className="text-xs text-muted-foreground">{NR_LAUNCH.POSITION}</dt>
                <dd className="font-medium">{placeInLine(queue)}</dd>
              </div>
              <div>
                <dt className="text-xs text-muted-foreground">{NR_LAUNCH.WAITING_SINCE}</dt>
                <dd className="font-medium">{waitingSince(queue.queued_at, now)}</dd>
              </div>
              <div>
                <dt className="text-xs text-muted-foreground">{NR_LAUNCH.GIVES_UP}</dt>
                <dd className="font-medium">{givesUp(queue.deadline_at, now)}</dd>
              </div>
            </dl>
          )}
          <Button type="button" variant="destructive" className="col-span-full" onClick={() => void kill(run.id)}>
            {NR_LAUNCH.CANCEL_RUN}
          </Button>
        </div>
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
    </section>
  );
}
