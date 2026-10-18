/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hero of a QUEUED run: why it waits, its place in line and when it gives up. The same
// sentences and figures the Launched screen shows, read from the run's own queue.

import { NR_LAUNCH } from "../../wardyn/copy/new-run/launch";
import type { RunQueue } from "../../../lib/types/runs";
import { givesUp, placeInLine, waitingBody, waitingSince } from "../new-run-flow/launch/launched-model";
import { useWaitingPool } from "../new-run-flow/launch/use-launched-run";

export function QueuedWait({ queue }: { queue: RunQueue }) {
  const pool = useWaitingPool(queue.pool_id);
  const body = waitingBody(queue.reason, pool);
  const now = Date.now();
  return (
    <div data-testid="run-queued-wait" className="flex min-h-0 flex-1 flex-col items-center justify-center gap-4 p-6 text-center">
      {body && <p className="max-w-md text-sm text-muted-foreground">{body}</p>}
      <dl className="grid grid-cols-3 gap-6 text-left text-xs">
        <div>
          <dt className="text-muted-foreground">{NR_LAUNCH.POSITION}</dt>
          <dd className="font-medium">{placeInLine(queue)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">{NR_LAUNCH.WAITING_SINCE}</dt>
          <dd className="font-medium">{waitingSince(queue.queued_at, now)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">{NR_LAUNCH.GIVES_UP}</dt>
          <dd className="font-medium">{givesUp(queue.deadline_at, now)}</dd>
        </div>
      </dl>
    </div>
  );
}
