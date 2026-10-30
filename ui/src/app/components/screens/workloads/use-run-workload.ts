/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { workloads } from "../../../lib/api/workloads";

/** The workload whose current run this is. A run's own record names no workload, so the run page asks the list once per run. */
export interface RunWorkload {
  name: string;
  owner: string;
}

/** A failed or refused read (a store with no workloads, a caller who may not list them) is a run no workload owns. */
export function useRunWorkload(runId: string | undefined): RunWorkload | undefined {
  const [found, setFound] = React.useState<{ runId: string; workload: RunWorkload | undefined } | null>(null);
  React.useEffect(() => {
    if (!runId) return;
    let live = true;
    workloads
      .list()
      .then((list) => {
        const w = list.find((x) => x.run_id === runId);
        if (live) setFound({ runId, workload: w && { name: w.name, owner: w.owner } });
      })
      .catch(() => live && setFound({ runId, workload: undefined }));
    return () => {
      live = false;
    };
  }, [runId]);
  return found && found.runId === runId ? found.workload : undefined;
}
