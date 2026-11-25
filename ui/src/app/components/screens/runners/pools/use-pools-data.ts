/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners Pools tab shows: every live pool (an administrator's list carries each person's
// user pools too), each read once more for its own raw limits, state and revision (a row the administrator
// may not USE carries only its id, name, scope and owner), and the organization default.
//
// The per-pool reads are bounded, because the list grows with people: the first load and a Retry read every
// pool, four at a time; the silent re-read (every 30 s while the tab is visible and the console is signed in, after a write, and when a
// pool's details open) asks only for the list, the default, any pool it has not read yet and the pool whose
// details are open. So a poll is two or three requests however many pools there are, the details open on
// the pool as it is now, and another administrator's change to a pool that is not open shows when its
// details or the tab are next opened. A failed silent read keeps what the tab already shows. A 501
// is its own state: the server says it has no pool storage and the tab shows that sentence, never an empty
// list.
// ponytail: the list itself should carry state, revision and limits for oversight rows; then no per-pool read.
import * as React from "react";
import { useQuery } from "@tanstack/react-query";
import { HttpError } from "../../../../lib/api/core";
import { runnerPools } from "../../../../lib/api/runner-pools";
import type { RunnerPool, RunnerPoolCatalogueEntry } from "../../../../lib/types/runner-pools";
import { queryClient } from "../../../../lib/query-client";
import { usePoll } from "../../../../lib/use-poll";

export const POLL_MS = 30_000;

const POOLS_KEY = ["runners", "pools"];
const POOLS_KEY_STR = JSON.stringify(POOLS_KEY);

export type PoolsStatus = "loading" | "ready" | "error" | "unavailable";

export interface PoolEntry {
  /** The catalogue row: slots (when the caller may use the pool) and the owner. */
  entry: RunnerPoolCatalogueEntry;
  /** The pool read on its own: state, revision and its own limits. */
  pool: RunnerPool;
}

export interface PoolsData {
  status: PoolsStatus;
  /** The server's own sentence while status is "unavailable". */
  message: string;
  entries: PoolEntry[];
  /** The organization default pool, when one is set. */
  defaultId: string | undefined;
}

const EMPTY: PoolsData = { status: "loading", message: "", entries: [], defaultId: undefined };

/** How many per-pool reads run at once on a full load. */
const WIDE = 4;

/** Runs task over items, at most WIDE at a time, keeping their order. */
async function bounded<T, R>(items: T[], task: (item: T) => Promise<R>): Promise<R[]> {
  const out = new Array<R>(items.length);
  let next = 0;
  const worker = async () => {
    for (let i = next++; i < items.length; i = next++) out[i] = await task(items[i]);
  };
  await Promise.all(Array.from({ length: Math.min(WIDE, items.length) }, worker));
  return out;
}

/**
 * Reads the list and the default, and each pool's own record: all of them when `known` is undefined (a full
 * load), otherwise only a pool not in `known` and the one named `open`.
 */
async function readAll(known: Map<string, RunnerPool> | undefined, open: string | null): Promise<Pick<PoolsData, "entries" | "defaultId">> {
  const [list, defaults] = await Promise.all([runnerPools.list(), runnerPools.orgDefaults()]);
  // A pool deleted between the list and its own read is gone, not an error.
  const read = await bounded(list.pools, async (entry): Promise<PoolEntry | null> => {
    const have = known?.get(entry.id);
    if (have && entry.id !== open) return { entry, pool: have };
    try {
      return { entry, pool: await runnerPools.get(entry.id) };
    } catch (e) {
      if (e instanceof HttpError && e.status === 404) return null;
      throw e;
    }
  });
  return { entries: read.filter((r): r is PoolEntry => r !== null), defaultId: defaults.pool_id };
}

export function usePoolsData(openPoolId: string | null): PoolsData & { reload: () => void; retry: () => void } {
  // What the last read knew, and the open pool, as the next silent read sees them.
  const known = React.useRef(new Map<string, RunnerPool>());
  const open = React.useRef(openPoolId);
  open.current = openPoolId;
  // The first read, and a Retry, read every pool; any other read asks only for what is new and the open pool.
  const full = React.useRef(true);

  // One key for the whole tab: the list, the default and each pool's own record are one answer.
  const q = useQuery(
    {
      queryKey: POOLS_KEY,
      queryFn: () => {
        const wantFull = full.current;
        full.current = false;
        return readAll(wantFull ? undefined : known.current, open.current);
      },
    },
    queryClient,
  );
  React.useEffect(() => {
    if (q.data) known.current = new Map(q.data.entries.map((e) => [e.pool.id, e.pool]));
  }, [q.data]);

  // The first failure stays on screen: a later silent read that fails never replaces it, and only an answer clears it.
  const [held, setHeld] = React.useState<{ key: string; error: unknown } | null>(null);
  const [retrying, setRetrying] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (q.isSuccess) setHeld(null);
    else if (q.isError) setHeld((h) => (h?.key === POOLS_KEY_STR ? h : { key: POOLS_KEY_STR, error: q.error }));
  }, [q.isSuccess, q.isError, q.error]);
  const err = held?.key === POOLS_KEY_STR ? held.error : q.isError ? q.error : null;

  // With no data a read in flight is cancelled, not joined, so a hung first read never holds the tab on loading.
  // During a Retry the tick joins the Retry's read instead, so the Retry's loading lasts until its answer.
  const tick = () =>
    retrying === POOLS_KEY_STR
      ? q.refetch({ cancelRefetch: false })
      : queryClient.getQueryData(POOLS_KEY) === undefined
        ? queryClient.cancelQueries({ queryKey: POOLS_KEY, exact: true }).then(() => q.refetch())
        : q.refetch({ cancelRefetch: false });
  usePoll(tick, POLL_MS, false);

  // Opening a pool's details reads that pool then, not at the next tick: its state and revision are what the
  // sheet shows and what Switch off sends back. The first load has just read every pool.
  const { refetch } = q;
  const mounted = React.useRef(false);
  React.useEffect(() => {
    if (mounted.current && openPoolId) void refetch();
    mounted.current = true;
  }, [openPoolId, refetch]);

  const reload = React.useCallback(() => void refetch(), [refetch]);
  const retry = React.useCallback(() => {
    full.current = true;
    setHeld(null);
    setRetrying(POOLS_KEY_STR);
    void queryClient
      .cancelQueries({ queryKey: POOLS_KEY, exact: true })
      .then(() => refetch())
      .then((r) => {
        if (r.isError) setHeld({ key: POOLS_KEY_STR, error: r.error });
      })
      .finally(() => setRetrying(null));
  }, [refetch]);

  if (retrying === POOLS_KEY_STR) return { ...EMPTY, ...q.data, status: "loading", reload, retry };
  if (q.data !== undefined) return { ...EMPTY, ...q.data, status: "ready", reload, retry };
  if (err instanceof HttpError && err.status === 501) return { ...EMPTY, status: "unavailable", message: err.message, reload, retry };
  return { ...EMPTY, status: err !== null ? "error" : "loading", reload, retry };
}
