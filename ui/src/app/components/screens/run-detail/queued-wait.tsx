/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero of a QUEUED run: why it waits, its place in line and when it gives up. The same
// sentences and figures the Launched screen shows, read from the run's own queue. A scheduled run
// waits for its time, and once due may wait for its owner's sign-in: those two have heroes of their own.

import * as React from "react";
import { CalendarClock, Clock, User } from "lucide-react";
import { Button } from "../../ui/button";
import { NR_LAUNCH } from "../../wardyn/copy/new-run/launch";
import { RUN_COCKPIT as RC } from "../../wardyn/copy/run-cockpit";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { SCHEDULED_RUN } from "../../wardyn/copy/scheduled-runs";
import { useClaimModelAccessDoor, useModelAccessDoor, useShellSetupStatus } from "../../wardyn/model-access-context";
import { useConsoleMode } from "../../wardyn/console-view";
import { resolveDoor } from "../../../lib/model-access";
import { SIGN_IN_AGAIN } from "../../../lib/session-renew-copy";
import type { RunDetail, RunQueue } from "../../../lib/types";
import { givesUp, placeInLine, waitingBody, waitingSince } from "../new-run-flow/launch/launched-model";
import { useWaitingPool } from "../new-run-flow/launch/use-launched-run";
import { scheduledWait } from "../runs/scheduled-state";
import CancelRunDialog from "./cancel-run-dialog";
import { signInBody, startsLine } from "./scheduled-lines";

const FRAME = "flex min-h-0 flex-1 flex-col items-center justify-center gap-2.5 p-6 text-center";

export function QueuedWait({ queue, run }: { queue: RunQueue; run?: RunDetail }) {
  const kind = run && scheduledWait({ state: "QUEUED", queue });
  if (run && kind === "scheduled") return <ScheduledWait queue={queue} run={run} />;
  if (run && kind === "sign_in") return <SignInWait queue={queue} run={run} />;
  return <PoolWait queue={queue} />;
}

function PoolWait({ queue }: { queue: RunQueue }) {
  const pool = useWaitingPool(queue.pool_id);
  const body = waitingBody(queue.reason, pool);
  const now = Date.now();
  const since = waitingSince(queue.queued_at, now);
  const gives = givesUp(queue.deadline_at, now);
  const inPrefix = NR_LAUNCH.GIVES_UP_IN("");
  const line = RC.queuedLine(
    placeInLine(queue),
    since === NR_LAUNCH.JUST_NOW ? RC.queuedJustJoined : RC.queuedSince(since),
    gives.startsWith(inPrefix) ? RC.queuedGivesUpIn(gives.slice(inPrefix.length)) : RC.queuedGivesUpAt(gives),
  );
  return (
    <div data-testid="run-queued-wait" className={FRAME}>
      <Clock className="size-7 text-muted-foreground" strokeWidth={1.5} aria-hidden />
      <h3 className="text-xl font-semibold text-foreground">{RC.queuedTitle}</h3>
      {body && <p className="max-w-md text-sm text-muted-foreground">{body}</p>}
      <p data-testid="run-queued-wait-line" className="text-sm text-foreground">{line}</p>
    </div>
  );
}

/** Cancel run, with today's confirm. */
function CancelRun({ run }: { run: RunDetail }) {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Button type="button" variant="outline" onClick={() => setOpen(true)}>{SCHEDULED_RUN.CANCEL_RUN}</Button>
      {open && <CancelRunDialog runId={run.id} title={run.title?.trim() || run.task} onClose={() => setOpen(false)} />}
    </>
  );
}

function ScheduledWait({ queue, run }: { queue: RunQueue; run: RunDetail }) {
  const starts = startsLine(queue);
  return (
    <div data-testid="run-scheduled-wait" className={FRAME}>
      <CalendarClock className="size-7 text-muted-foreground" strokeWidth={1.5} aria-hidden />
      <h3 className="text-xl font-semibold text-foreground">{SCHEDULED_RUN.WORD}</h3>
      {starts && <p className="text-sm text-foreground">{starts}</p>}
      <p className="max-w-md text-sm text-muted-foreground">{SCHEDULED_RUN.PAGE_NOTE}</p>
      <div className="mt-1.5"><CancelRun run={run} /></div>
    </div>
  );
}

function SignInWait({ queue, run }: { queue: RunQueue; run: RunDetail }) {
  const door = useModelAccessDoor();
  const { status } = useShellSetupStatus();
  const view = useConsoleMode();
  // Only the run's owner, in the User view, has a door that serves it: a sign-in lands in the signer's own namespace.
  const provider = run.model_provider_id ?? "";
  const yours = !!door.principal && run.created_by === door.principal && view === "user";
  const target = yours && provider ? resolveDoor(status, { provider }, "user") : null;
  useClaimModelAccessDoor(!!target);
  const body = signInBody(queue);
  return (
    <div data-testid="run-sign-in-wait" className={FRAME}>
      <User className="size-7 text-warning" strokeWidth={1.5} aria-hidden />
      <h3 className="text-xl font-semibold text-foreground">{SCHEDULED_RUN.SIGN_IN_TITLE}</h3>
      {run.model_provider_name && <p className="text-sm text-foreground">{CONNECTIONS.C6_LINE(run.model_provider_name)}</p>}
      {body && <p className="max-w-md text-sm text-muted-foreground">{body}</p>}
      <div className="mt-1.5 flex gap-2">
        {target && (
          <Button type="button" onClick={(e) => door.openDoor({ for: { provider }, returnTo: e.currentTarget })}>
            {SIGN_IN_AGAIN}
          </Button>
        )}
        <CancelRun run={run} />
      </div>
    </div>
  );
}
