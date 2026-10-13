/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one read the Run step makes: GET /runner-pools for the run type asked. A change of run type asks
// again, and the answer for the other type is never shown meanwhile.

import * as React from "react";
import { asJson, wfetch } from "../../../../../lib/api/core";
import type { RunnerPoolList, RunnerPoolRunType } from "../../../../../lib/types/runner-pools";

export type LoadPools = (runType: RunnerPoolRunType | undefined, signal: AbortSignal) => Promise<RunnerPoolList>;

// ponytail: runnerPools.list() takes no run_type yet; fold this into it when that module is open.
export const loadPools: LoadPools = async (runType, signal) =>
  asJson<RunnerPoolList>(await wfetch(`/runner-pools${runType ? `?run_type=${runType}` : ""}`, { method: "GET", signal }));

export type PoolsState = { status: "loading" } | { status: "error"; message: string } | { status: "ready"; list: RunnerPoolList };

export function usePools(load: LoadPools, runType: RunnerPoolRunType | undefined): PoolsState {
  const [answer, setAnswer] = React.useState<{ runType: RunnerPoolRunType | undefined; state: PoolsState }>();
  React.useEffect(() => {
    const controller = new AbortController();
    load(runType, controller.signal).then(
      (list) => !controller.signal.aborted && setAnswer({ runType, state: { status: "ready", list } }),
      (error: unknown) => !controller.signal.aborted && setAnswer({ runType, state: { status: "error", message: error instanceof Error ? error.message : String(error) } }),
    );
    return () => controller.abort();
  }, [load, runType]);
  return answer && answer.runType === runType ? answer.state : { status: "loading" };
}
