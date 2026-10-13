/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners Pools tab shows: every live pool (an administrator's list carries each person's
// user pools too), each read once more for its own raw limits, state and revision (a row the administrator
// may not USE carries only its id, name, scope and owner), and the organization default.
//
// The per-pool reads are bounded, because the list grows with people: the first load and a Retry read every
// pool, four at a time; the silent re-read (every 30 s while the tab is visible, after a write, and when a
// pool's details open) asks only for the list, the default, any pool it has not read yet and the pool whose
// details are open. So a poll is two or three requests however many pools there are, the details open on
// the pool as it is now, and another administrator's change to a pool that is not open shows when its
// details or the tab are next opened. A failed silent read keeps what the tab already shows. A 501
// is its own state: the server says it has no pool storage and the tab shows that sentence, never an empty
// list.
// ponytail: the list itself should carry state, revision and limits for oversight rows; then no per-pool read.
import * as React from "react";
import { HttpError } from "../../../../lib/api/core";
import { runnerPools } from "../../../../lib/api/runner-pools";
import type { RunnerPool, RunnerPoolCatalogueEntry } from "../../../../lib/types/runner-pools";

export const POLL_MS = 30_000;

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
  const [data, setData] = React.useState<PoolsData>(EMPTY);
  const seq = React.useRef(0);
  // What the last read knew, and the open pool, as the next silent read sees them.
  const known = React.useRef(new Map<string, RunnerPool>());
  const open = React.useRef(openPoolId);
  open.current = openPoolId;

  // A read that was in flight when the tab went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    if (!silent) setData((d) => ({ ...d, status: "loading" }));
    readAll(silent ? known.current : undefined, open.current)
      .then((r) => {
        if (mine !== seq.current) return;
        known.current = new Map(r.entries.map((e) => [e.pool.id, e.pool]));
        setData({ status: "ready", message: "", ...r });
      })
      .catch((e: unknown) => {
        if (mine !== seq.current || silent) return;
        if (e instanceof HttpError && e.status === 501) setData({ ...EMPTY, status: "unavailable", message: e.message });
        else setData((d) => ({ ...d, status: "error" }));
      });
  }, []);

  React.useEffect(() => {
    read(false);
    const tick = () => document.visibilityState === "visible" && read(true);
    const id = setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
      abandon();
    };
  }, [read, abandon]);

  // Opening a pool's details reads that pool then, not at the next tick: its state and revision are what the
  // sheet shows and what Switch off sends back. The first load has just read every pool.
  const mounted = React.useRef(false);
  React.useEffect(() => {
    if (mounted.current && openPoolId) read(true);
    mounted.current = true;
  }, [openPoolId, read]);

  const reload = React.useCallback(() => read(true), [read]);
  const retry = React.useCallback(() => read(false), [read]);
  return { ...data, reload, retry };
}
