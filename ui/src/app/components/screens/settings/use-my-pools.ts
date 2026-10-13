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
import { HttpError } from "../../../lib/api/core";
import { remoteRunners, runnerPools } from "../../../lib/api/runner-pools";
import type { RemoteRunnerView, RunnerPool, RunnerPoolCatalogueEntry, RunnerPoolMemberView } from "../../../lib/types/runner-pools";
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

const EMPTY: MyPoolsData = { status: "loading", pools: [], defaultId: null, defaultGone: false, remote: [] };

export function useMyPools(enabled: boolean): MyPoolsData & { reload: () => void } {
  const me = usePrincipal();
  const oversees = useSecurityOperator();
  const [data, setData] = React.useState<MyPoolsData>(EMPTY);
  const seq = React.useRef(0);
  const read = React.useCallback(async () => {
    const mine = ++seq.current;
    try {
      const [list, remote] = await Promise.all([runnerPools.list(), remoteRunners.listMine()]);
      const own = list.pools.filter((p) => p.scope === "user" && (!oversees || p.owner === me));
      const pools = await Promise.all(
        own.map(async (entry): Promise<MyPool> => {
          const [pool, members] = await Promise.all([runnerPools.get(entry.id), runnerPools.members(entry.id)]);
          return { entry, pool, members };
        }),
      );
      if (mine !== seq.current) return;
      const def = list.default;
      setData({
        status: "ready",
        pools,
        remote,
        defaultId: def?.selection === "personal_default" && def.pool_id && !def.reason && pools.some((p) => p.pool.id === def.pool_id) ? def.pool_id : null,
        defaultGone: def?.selection === "personal_default" && !!def.reason,
      });
    } catch (e) {
      if (mine !== seq.current) return;
      setData((d) => ({ ...d, status: e instanceof HttpError && e.status === 501 ? "unavailable" : "error" }));
    }
  }, [me, oversees]);
  // A read that was in flight when the card went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);
  React.useEffect(() => {
    if (enabled) void read();
    return abandon;
  }, [enabled, read, abandon]);
  const reload = React.useCallback(() => void read(), [read]);
  return { ...data, reload };
}
