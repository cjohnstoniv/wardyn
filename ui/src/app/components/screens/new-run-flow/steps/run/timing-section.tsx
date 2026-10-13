/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { Chip } from "../../../../wardyn/primitives";

/** When an Autonomous run starts. Scheduling is Coming soon: no start time exists on the wire, so Run Now is the only choice. */
export function TimingSection() {
  const labelId = React.useId();
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <span id={labelId} className="label-eyebrow">{NR_RUN.TIMING_LABEL}</span>
        <Chip>{NR_SHARED.REQUIRED}</Chip>
      </div>
      <RadioCardGroup name="nr-run-timing" aria-labelledby={labelId} value="now" onValueChange={() => {}} className="grid gap-3 sm:grid-cols-2">
        <RadioCard value="now" title={NR_RUN.TIMING_NOW} description={NR_RUN.TIMING_NOW_BODY} />
        <RadioCard value="later" title={NR_RUN.TIMING_LATER} description={NR_RUN.TIMING_LATER_BODY} unavailable={{ reason: NR_SHARED.COMING_SOON }} />
      </RadioCardGroup>
    </div>
  );
}
