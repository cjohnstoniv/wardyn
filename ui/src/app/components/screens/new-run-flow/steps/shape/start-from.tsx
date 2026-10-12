/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import type { TemplateSummary } from "../../../../../lib/types/templates";
import type { LaunchIntentSummary } from "../../../../../lib/new-run-fixtures/scenarios/templates";
import type { GuidedFacts, NewRunOrigin } from "../../draft/run-intent";
import { AgainPicker } from "./again-picker";
import { TemplatePicker } from "./template-picker";

/** The radio's value is the draft's `guided.startFrom`; a picker follows the chosen source. */
export function StartFrom({ startFrom, origin, onSource, onPickTemplate, onPickAgain }: {
  startFrom: GuidedFacts["startFrom"];
  origin: NewRunOrigin;
  onSource: (source: GuidedFacts["startFrom"]) => void;
  onPickTemplate: (template: TemplateSummary) => void;
  onPickAgain: (intent: LaunchIntentSummary) => void;
}) {
  const labelId = React.useId();
  return (
    <div className="flex flex-col gap-3">
      <p id={labelId} className="label-eyebrow">{NR_SHAPE.START_FROM}</p>
      <RadioCardGroup name="nr-start-from" aria-labelledby={labelId} value={startFrom} onValueChange={(v) => onSource(v as GuidedFacts["startFrom"])} className="grid gap-2 sm:grid-cols-3">
        <RadioCard value="scratch" title={NR_SHAPE.SOURCE_SCRATCH} />
        <RadioCard value="template" title={NR_SHAPE.SOURCE_TEMPLATE} />
        <RadioCard value="again" title={NR_SHAPE.SOURCE_AGAIN} />
      </RadioCardGroup>
      {startFrom === "template" && <TemplatePicker selectedId={origin.kind === "template" ? origin.id : undefined} onPick={onPickTemplate} />}
      {startFrom === "again" && <AgainPicker selectedRunId={origin.kind === "again" ? origin.runId : undefined} onPick={onPickAgain} />}
    </div>
  );
}
