/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { Label } from "../../../../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../../../ui/select";
import { relativeTime } from "../../../../../lib/format";
import type { LaunchIntentSummary } from "../../../../../lib/new-run-fixtures/scenarios/templates";
import { launchIntentSource } from "./shape-sources";
import { useLoaded } from "./use-loaded";

const list = (signal: AbortSignal) => launchIntentSource.list(signal);

/** The creator's own runs that kept a setup. Choosing one opens Review on it. */
export function AgainPicker({ selectedRunId, onPick }: { selectedRunId?: string; onPick: (intent: LaunchIntentSummary) => void }) {
  const loaded = useLoaded(list);
  if (loaded.status === "error") return <p role="alert" className="text-sm text-danger">{loaded.message}</p>;
  if (loaded.status === "ready" && loaded.value.intents.length === 0) {
    return <p className="text-sm text-muted-foreground">{NR_SHAPE.EMPTY_AGAIN}</p>;
  }
  const intents = loaded.status === "ready" ? loaded.value.intents : [];
  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor="nr-previous-run">{NR_SHAPE.PREVIOUS_LABEL}</Label>
      <Select
        value={selectedRunId ?? ""}
        disabled={loaded.status === "loading"}
        onValueChange={(runId) => {
          const picked = intents.find((i) => i.run_id === runId);
          if (picked) onPick(picked);
        }}
      >
        <SelectTrigger id="nr-previous-run" aria-busy={loaded.status === "loading"} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {intents.map((i) => (
            <SelectItem key={i.run_id} value={i.run_id}>
              {NR_SHAPE.AGAIN_OPTION(i.title, relativeTime(i.created_at))}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="text-xs text-muted-foreground">{NR_SHAPE.AGAIN_NOTE}</p>
    </div>
  );
}
