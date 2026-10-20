/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero of a QUEUED run: why it waits, its place in line and when it gives up. The same
// sentences and figures the Launched screen shows, read from the run's own queue.

import { Clock } from "lucide-react";
import { NR_LAUNCH } from "../../wardyn/copy/new-run/launch";
import { RUN_COCKPIT as RC } from "../../wardyn/copy/run-cockpit";
import type { RunQueue } from "../../../lib/types/runs";
import { givesUp, placeInLine, waitingBody, waitingSince } from "../new-run-flow/launch/launched-model";
import { useWaitingPool } from "../new-run-flow/launch/use-launched-run";

export function QueuedWait({ queue }: { queue: RunQueue }) {
  const pool = useWaitingPool(queue.pool_id);
  const body = waitingBody(queue.reason, pool, queue.start_at);
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
    <div data-testid="run-queued-wait" className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2.5 p-6 text-center">
      <Clock className="size-7 text-muted-foreground" strokeWidth={1.5} aria-hidden />
      <h3 className="text-xl font-semibold text-foreground">{RC.queuedTitle}</h3>
      {body && <p className="max-w-md text-sm text-muted-foreground">{body}</p>}
      <p data-testid="run-queued-wait-line" className="text-sm text-foreground">{line}</p>
    </div>
  );
}
