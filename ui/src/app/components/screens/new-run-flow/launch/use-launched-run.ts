/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Launched screen's reads: the run by its id (the 201 carries only a state and
// a sentence, so the queue is read from the run), kept fresh while it is still
// starting or waiting, and the name and limit of the pool it waits for.

import * as React from "react";
import { runs } from "../../../../lib/api/runs";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { usePoll } from "../../../../lib/use-poll";
import type { LaunchedRun } from "./launched-model";

/** Run detail's cadence. */
export const LAUNCHED_POLL_MS = 4000;
const LIVE = new Set(["PENDING", "STARTING", "QUEUED"]);
export const stillLaunching = (run: LaunchedRun | null | undefined): boolean => !run || LIVE.has(run.state as string);

export interface WaitingPool {
  name: string;
  max?: number;
}

export interface LaunchedRead {
  /** undefined: not read yet. null: the server has no such run. */
  run: LaunchedRun | null | undefined;
  failed: boolean;
  now: number;
  reload: () => void;
  pool: WaitingPool | undefined;
}

/** The name and limit of the pool a queued run waits for; undefined until the pool list answers. */
export function useWaitingPool(poolId: string | undefined): WaitingPool | undefined {
  const [pool, setPool] = React.useState<WaitingPool>();
  React.useEffect(() => {
    setPool(undefined);
    if (!poolId) return;
    let live = true;
    runnerPools.list().then(
      (list) => {
        const row = list.pools.find((p) => p.id === poolId);
        if (live && row) setPool({ name: row.name, max: row.slots?.max });
      },
      () => {},
    );
    return () => {
      live = false;
    };
  }, [poolId]);
  return pool;
}

/** `seed` is what a create just answered with; the run is read at once either way. */
export function useLaunchedRun(runId: string, seed?: LaunchedRun): LaunchedRead {
  const [run, setRun] = React.useState<LaunchedRun | null | undefined>(seed);
  const [failed, setFailed] = React.useState(false);
  const [now, setNow] = React.useState(Date.now);
  const current = React.useRef(runId);
  current.current = runId;

  const read = React.useCallback(async () => {
    try {
      const next = await runs.getRun(runId);
      if (current.current !== runId) return;
      setRun(next ?? null);
      setFailed(false);
    } catch {
      if (current.current === runId) setFailed(true);
    }
    if (current.current === runId) setNow(Date.now());
  }, [runId]);

  React.useEffect(() => {
    setRun(seed);
    void read();
    // The seed only starts the screen; the id is what a read is for.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId]);
  usePoll(read, LAUNCHED_POLL_MS, !failed && !stillLaunching(run));

  const pool = useWaitingPool(run?.queue?.pool_id);

  return { run, failed, now, reload: () => void read(), pool };
}
