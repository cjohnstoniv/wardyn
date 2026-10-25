/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one read the Run step makes: GET /runner-pools for the run type asked. A change of run type asks
// again (and no `load` asks nothing), and the answer for the other type is never shown meanwhile. A change of
// `refresh` asks again for the same type: the answer in hand stays until the new one arrives, and a failed ask
// does not replace it.

import * as React from "react";
import { runnerPools } from "../../../../../lib/api/runner-pools";
import type { RunnerPoolList, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";

export type LoadPools = (runType: RunnerPoolRunType | undefined, signal: AbortSignal) => Promise<RunnerPoolList>;

export const loadPools: LoadPools = (runType, signal) => runnerPools.list(runType, signal);

export type PoolsState = { status: "loading" } | { status: "error"; message: string } | { status: "ready"; list: RunnerPoolList };

export function usePools(load: LoadPools | undefined, runType: RunnerPoolRunType | undefined, refresh?: string): PoolsState {
  const [answer, setAnswer] = React.useState<{ runType: RunnerPoolRunType | undefined; state: PoolsState }>();
  React.useEffect(() => {
    if (!load) return;
    const controller = new AbortController();
    load(runType, controller.signal).then(
      (list) => !controller.signal.aborted && setAnswer({ runType, state: { status: "ready", list } }),
      (error: unknown) =>
        !controller.signal.aborted &&
        setAnswer((held) =>
          held?.runType === runType && held.state.status === "ready"
            ? held
            : { runType, state: { status: "error", message: error instanceof Error ? error.message : String(error) } },
        ),
    );
    return () => controller.abort();
  }, [load, runType, refresh]);
  return answer && answer.runType === runType ? answer.state : { status: "loading" };
}
