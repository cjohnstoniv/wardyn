/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { QuestionBlock } from "./question-block";
import type { RunMode } from "../../lenses/shape-lens";

export const MODE_ID = "nr-mode";

/** How the agent runs: Interactive or Autonomous. A draft with no mode yet shows Interactive, which the step then commits. */
export function ModeSection({ mode, onChange }: { mode: RunMode; onChange: (mode: RunMode) => void }) {
  const labelId = React.useId();
  return (
    <QuestionBlock id={MODE_ID} labelId={labelId} label={NR_RUN.MODE_LABEL}>
      <RadioCardGroup name="nr-run-mode" aria-labelledby={labelId} value={mode} onValueChange={(v) => onChange(v as RunMode)} className="grid gap-3 sm:grid-cols-2">
        <RadioCard value="interactive" title={NR_RUN.MODE_INTERACTIVE} description={NR_RUN.MODE_INTERACTIVE_BODY} />
        <RadioCard value="autonomous" title={NR_RUN.MODE_AUTONOMOUS} description={NR_RUN.MODE_AUTONOMOUS_BODY} />
      </RadioCardGroup>
    </QuestionBlock>
  );
}
