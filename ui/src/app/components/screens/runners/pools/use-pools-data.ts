/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners Pools tab shows: every live pool (an administrator's list carries each person's
// user pools too), each read once more for its own raw limits, state and revision (a row the administrator
// may not USE carries only its id, name, scope and owner), and the organization default. Re-read silently
// every 30 s while the tab is visible and when it becomes visible again; a failed silent read keeps what the
// tab already shows. A 501 is its own state: the server says it has no pool storage and the tab shows that
// sentence, never an empty list.
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

async function readAll(): Promise<Pick<PoolsData, "entries" | "defaultId">> {
  const [list, defaults] = await Promise.all([runnerPools.list(), runnerPools.orgDefaults()]);
  // A pool deleted between the list and its own read is gone, not an error.
  const read = await Promise.all(
    list.pools.map((entry) =>
      runnerPools.get(entry.id).then(
        (pool): PoolEntry => ({ entry, pool }),
        (e: unknown) => {
          if (e instanceof HttpError && e.status === 404) return null;
          throw e;
        },
      ),
    ),
  );
  return { entries: read.filter((r): r is PoolEntry => r !== null), defaultId: defaults.pool_id };
}

export function usePoolsData(): PoolsData & { reload: () => void; retry: () => void } {
  const [data, setData] = React.useState<PoolsData>(EMPTY);
  const seq = React.useRef(0);

  // A read that was in flight when the tab went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    if (!silent) setData((d) => ({ ...d, status: "loading" }));
    readAll()
      .then((r) => {
        if (mine === seq.current) setData({ status: "ready", message: "", ...r });
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

  const reload = React.useCallback(() => read(true), [read]);
  const retry = React.useCallback(() => read(false), [read]);
  return { ...data, reload, retry };
}
