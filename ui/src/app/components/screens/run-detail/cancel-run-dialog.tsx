/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cancel run on a run that waits: today's kill confirm, then the kill. It is open from the moment it mounts, so a
// caller mounts it on the press and unmounts it on `onClose`. The Runs row loads it lazily; the run page's hero mounts it.

import { toast } from "sonner";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { KillRunDialog } from "../../wardyn/kill-run-dialog";

export default function CancelRunDialog({ runId, title, onClose }: { runId: string; title?: string; onClose: () => void }) {
  const cancel = async (id: string) => {
    onClose();
    try {
      await runsApi.killRun(id);
      toast.success(`Kill requested for ${id}`);
    } catch (err) {
      toast.error(`Failed to kill ${id}`, { description: getErrorMessage(err) });
    }
  };
  return <KillRunDialog runId={runId} title={title} onOpenChange={(open) => !open && onClose()} onConfirm={(id) => void cancel(id)} />;
}
