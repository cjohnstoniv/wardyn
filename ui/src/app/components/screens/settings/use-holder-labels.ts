/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runs } from "../../../lib/api/runs";
import type { KeptMachine } from "../../../lib/types";

export const shortRunId = (id: string) => id.replace(/^run_/, "").slice(0, 8);

/** The title of each run that holds a machine, else its short id: K-56 names the run the way the server's sentences do. */
export function useHolderLabels(machines: readonly KeptMachine[]): Record<string, string> {
  const [labels, setLabels] = React.useState<Record<string, string>>({});
  const ids = machines.flatMap((m) => ((m.state === "in_use" || m.state === "creating") && m.run_id ? [m.run_id] : []));
  const key = ids.join(",");
  React.useEffect(() => {
    let live = true;
    for (const id of key ? key.split(",") : []) {
      runs
        .getRun(id)
        .then((r) => live && setLabels((l) => ({ ...l, [id]: r?.title?.trim() || shortRunId(id) })))
        .catch(() => {});
    }
    return () => {
      live = false;
    };
  }, [key]);
  return labels;
}
