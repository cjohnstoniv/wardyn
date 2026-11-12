/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cancel run on a run that has not started (scheduled, or waiting for its owner's sign-in): a plain cancel confirm, then the kill. It is open from the moment it mounts, so a
// caller mounts it on the press and unmounts it on `onClose`. The Runs row loads it lazily; the run page's hero mounts it.

import { toast } from "sonner";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { CANCEL_NOT_STARTED as NS } from "../../wardyn/copy/cancel-not-started";
import { toastRunName } from "../../wardyn/copy/run-lifetime";
import { KillRunDialog } from "../../wardyn/kill-run-dialog";

export default function CancelRunDialog({ runId, title, onClose }: { runId: string; title?: string; onClose: () => void }) {
  // The toast names the run by its title, else its task, never its id; with neither it says "this run".
  const name = toastRunName({ title });
  const cancel = async (id: string) => {
    onClose();
    try {
      await runsApi.killRun(id);
      toast.success(NS.DONE(name));
    } catch (err) {
      toast.error(NS.FAILED(name), { description: getErrorMessage(err) });
    }
  };
  return <KillRunDialog runId={runId} title={title} onOpenChange={(open) => !open && onClose()} onConfirm={(id) => void cancel(id)} notStarted />;
}
