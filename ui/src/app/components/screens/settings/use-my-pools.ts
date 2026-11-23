/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What Your account -> My pools shows: the caller's own user pools, each with its own limits and its
// members, their personal default, and the remote-hosted runners an admin made available to people's
// pools. An administrator's pool list also carries every other person's user pools for oversight, so
// theirs are told apart by owner. A server with no pool storage answers 501, which this reports as
// "unavailable" rather than as an empty list.
import * as React from "react";
import { useQuery } from "@tanstack/react-query";
import { HttpError } from "../../../lib/api/core";
import { remoteRunners, runnerPools } from "../../../lib/api/runner-pools";
import type { RemoteRunnerView, RunnerPool, RunnerPoolCatalogueEntry, RunnerPoolMemberView } from "../../../lib/types/runner-pools";
import { queryClient } from "../../../lib/query-client";
import { usePrincipal, useSecurityOperator } from "../../wardyn/operator-context";

export interface MyPool {
  entry: RunnerPoolCatalogueEntry;
  pool: RunnerPool;
  members: RunnerPoolMemberView[];
}

export interface MyPoolsData {
  status: "loading" | "ready" | "error" | "unavailable";
  pools: MyPool[];
  /** The caller's default pool when it is one of their own. */
  defaultId: string | null;
  /** Their saved default no longer applies, so New Run asks them to choose. */
  defaultGone: boolean;
  /** The remote-hosted runners they may add to a pool of theirs. */
  remote: RemoteRunnerView[];
}

const MY_POOLS_KEY = ["settings", "my-pools"];
const MY_POOLS_KEY_STR = JSON.stringify(MY_POOLS_KEY);
const EMPTY: MyPoolsData = { status: "loading", pools: [], defaultId: null, defaultGone: false, remote: [] };

export function useMyPools(enabled: boolean): MyPoolsData & { reload: () => void } {
  const me = usePrincipal();
  const oversees = useSecurityOperator();
  // One key for the card: the shell's placeholder identity changes when /me answers, so the key leaves it out and
  // an identity change re-reads under the same key below, keeping the pools on screen.
  const q = useQuery(
    {
      queryKey: MY_POOLS_KEY,
      enabled,
      queryFn: async (): Promise<Omit<MyPoolsData, "status">> => {
        const [list, remote] = await Promise.all([runnerPools.list(), remoteRunners.listMine()]);
        const own = list.pools.filter((p) => p.scope === "user" && (!oversees || p.owner === me));
        const pools = await Promise.all(
          own.map(async (entry): Promise<MyPool> => {
            const [pool, members] = await Promise.all([runnerPools.get(entry.id), runnerPools.members(entry.id)]);
            return { entry, pool, members };
          }),
        );
        const def = list.default;
        return {
          pools,
          remote,
          defaultId: def?.selection === "personal_default" && def.pool_id && !def.reason && pools.some((p) => p.pool.id === def.pool_id) ? def.pool_id : null,
          defaultGone: def?.selection === "personal_default" && !!def.reason,
        };
      },
    },
    queryClient,
  );
  const { refetch } = q;

  const who = `${me}\u0000${oversees}`;
  const readFor = React.useRef(who);
  React.useEffect(() => {
    if (readFor.current === who) return;
    readFor.current = who;
    if (enabled) void queryClient.cancelQueries({ queryKey: MY_POOLS_KEY, exact: true }).then(() => refetch());
  }, [who, enabled, refetch]);

  // The last failure stays on screen while a re-read is in flight; only a read that answers replaces it.
  const [held, setHeld] = React.useState<{ key: string; error: unknown } | null>(null);
  React.useEffect(() => {
    if (q.isSuccess) setHeld(null);
    else if (q.isError) setHeld({ key: MY_POOLS_KEY_STR, error: q.error });
  }, [q.isSuccess, q.isError, q.error]);
  const err = q.isError ? q.error : held?.key === MY_POOLS_KEY_STR ? held.error : null;

  const reload = React.useCallback(() => void refetch(), [refetch]);
  const status: MyPoolsData["status"] =
    err !== null ? (err instanceof HttpError && err.status === 501 ? "unavailable" : "error") : q.data !== undefined ? "ready" : "loading";
  return { ...EMPTY, ...q.data, status, reload };
}
