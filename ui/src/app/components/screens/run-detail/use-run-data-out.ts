/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runs } from "../../../lib/api/runs";

export type RunDataOut = "allowed" | "review" | "blocked";

/**
 * The run's data_out rule as its recorded policy reads it, fetched once. A recorded spec with no data_out is
 * allowed. Unknown (undefined) while the read is out, when it fails, and for a policy not yet recorded or
 * never recorded: nothing is drawn for an unknown rule. `redacted` says only that some values were withheld
 * from this reader; data_out is never one of them, so it is ignored.
 */
export function useRunDataOut(runId: string, wanted = true): RunDataOut | undefined {
  const [rule, setRule] = React.useState<RunDataOut>();
  React.useEffect(() => {
    if (!wanted) return;
    let alive = true;
    runs.getPolicy(runId).then(
      (v) => alive && setRule(v.state === "recorded" ? (v.spec?.data_out ?? "allowed") : undefined),
      () => {},
    );
    return () => {
      alive = false;
    };
  }, [runId, wanted]);
  return wanted ? rule : undefined;
}
