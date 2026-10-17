/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { Label } from "../../../../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../../../ui/select";
import { relativeTime } from "../../../../../lib/format";
import type { LaunchIntentSummary } from "../../../../../lib/types/launch-intents";
import { launchIntentSource } from "./shape-sources";
import { INLINE_CHOICE, INLINE_LABEL, useShown } from "./inline-choice";
import { useLoaded } from "./use-loaded";

const list = (signal: AbortSignal) => launchIntentSource.list(signal);

/** The creator's own runs that kept a setup (Make App.tsx:1229-1241). The shown row is reported up as in TemplatePicker. */
export function AgainPicker({ selectedRunId, onPick }: { selectedRunId?: string; onPick: (intent: LaunchIntentSummary) => void }) {
  const loaded = useLoaded(list);
  const intents = loaded.status === "ready" ? loaded.value.intents : [];
  const shown = useShown(intents, (i) => i.run_id === selectedRunId, onPick);
  if (loaded.status === "error") return <div className={INLINE_CHOICE}><p role="alert" className="text-sm text-danger">{loaded.message}</p></div>;
  if (loaded.status === "ready" && intents.length === 0) {
    return <div className={INLINE_CHOICE}><p className="text-sm text-muted-foreground">{NR_SHAPE.EMPTY_AGAIN}</p></div>;
  }
  return (
    <div className={INLINE_CHOICE}>
      <Label htmlFor="nr-previous-run" className={INLINE_LABEL}>{NR_SHAPE.PREVIOUS_LABEL}</Label>
      <Select
        value={shown?.run_id ?? ""}
        disabled={loaded.status === "loading"}
        onValueChange={(runId) => {
          const picked = intents.find((i) => i.run_id === runId);
          if (picked) onPick(picked);
        }}
      >
        <SelectTrigger id="nr-previous-run" aria-busy={loaded.status === "loading"} className="w-full bg-card dark:bg-card">
          <SelectValue placeholder={loaded.status === "loading" ? NR_SHAPE.AGAIN_LOADING : undefined} />
        </SelectTrigger>
        <SelectContent>
          {intents.map((i) => (
            <SelectItem key={i.run_id} value={i.run_id}>
              {NR_SHAPE.AGAIN_OPTION(i.title, relativeTime(i.created_at))}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="mt-2 text-meta text-muted-foreground">{NR_SHAPE.AGAIN_NOTE}</p>
    </div>
  );
}
