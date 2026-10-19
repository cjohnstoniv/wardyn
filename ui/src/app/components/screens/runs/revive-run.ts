/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One revive and the toasts every revive button owes: the hosts it newly blocked, the
// "no answer yet" sentence when the wait ended before the runner replied, or the failure.

import { toast } from "sonner";
import { runs as runsApi } from "../../../lib/api/runs";
import { waitEnded } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { REVIVE_RESULT_UNKNOWN, revivePolicy } from "../../wardyn/copy/run-lifetime";

/** False only when the revive definitely failed; a wait that ended may have changed the run, so it reads true. */
export async function reviveWithToasts(runId: string, failure: string): Promise<boolean> {
  try {
    const res = await runsApi.reviveRun(runId);
    if (res.denied_added.length > 0) toast.info(revivePolicy(res.denied_added.length));
    return true;
  } catch (err) {
    if (waitEnded(err)) {
      toast.info(REVIVE_RESULT_UNKNOWN);
      return true;
    }
    toast.error(failure, { description: getErrorMessage(err) });
    return false;
  }
}
