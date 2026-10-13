/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { Chip } from "../../../../wardyn/primitives";
import type { RunMode } from "../../lenses/shape-lens";

export const MODE_ID = "nr-mode";

/** How the agent runs: Interactive or Autonomous. A draft with no mode yet shows Interactive, which the step then commits. */
export function ModeSection({ mode, onChange }: { mode: RunMode; onChange: (mode: RunMode) => void }) {
  const labelId = React.useId();
  return (
    <div id={MODE_ID} className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <span id={labelId} className="label-eyebrow">{NR_RUN.MODE_LABEL}</span>
        <Chip>{NR_SHARED.REQUIRED}</Chip>
      </div>
      <RadioCardGroup name="nr-run-mode" aria-labelledby={labelId} value={mode} onValueChange={(v) => onChange(v as RunMode)} className="grid gap-3 sm:grid-cols-2">
        <RadioCard value="interactive" title={NR_RUN.MODE_INTERACTIVE} description={NR_RUN.MODE_INTERACTIVE_BODY} />
        <RadioCard value="autonomous" title={NR_RUN.MODE_AUTONOMOUS} description={NR_RUN.MODE_AUTONOMOUS_BODY} />
      </RadioCardGroup>
    </div>
  );
}
