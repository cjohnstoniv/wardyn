/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Panel-level progress for the composer. While a compose/clarify request streams,
// this shows a spinner + the current pipeline stage in plain language (mapped from
// the internal stage key by stageLabel). Honest: it reflects the real stage the
// server just emitted over SSE — not a client-side timer.
import { Loader2 } from "lucide-react";

import { stageLabel } from "../../../lib/compose-stages";

export function ComposeProgress({ stage }: { stage?: string }) {
  return (
    <div className="flex items-center gap-3 rounded-lg border border-border bg-muted/30 p-4">
      <Loader2 className="size-5 shrink-0 animate-spin text-primary" />
      <div className="min-w-0">
        <div className="text-sm font-medium text-foreground">{stageLabel(stage)}</div>
        <div className="text-[0.6875rem] text-muted-foreground">Composing your sandbox…</div>
      </div>
    </div>
  );
}
