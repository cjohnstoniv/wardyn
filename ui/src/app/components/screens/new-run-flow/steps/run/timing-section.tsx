/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { QuestionBlock } from "./question-block";

/** When an Autonomous run starts. Scheduling is not built and no start time exists on the wire, so the one answer is a sentence, not a radio (#3045). */
export function TimingSection() {
  const labelId = React.useId();
  return (
    <QuestionBlock labelId={labelId} label={NR_RUN.TIMING_LABEL}>
      <p aria-labelledby={labelId} className="text-sm">
        <strong className="font-medium">{NR_RUN.TIMING_NOW}</strong>
        <span className="text-muted-foreground"> · {NR_RUN.TIMING_NOW_BODY}</span>
      </p>
    </QuestionBlock>
  );
}
