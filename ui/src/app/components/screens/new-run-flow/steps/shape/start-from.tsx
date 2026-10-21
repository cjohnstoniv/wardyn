/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { StepSection } from "../../../../wardyn/step-section";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { cn } from "../../../../ui/utils";
import type { TemplateSummary } from "../../../../../lib/types/templates";
import type { LaunchIntentSummary } from "../../../../../lib/types/launch-intents";
import type { GuidedFacts } from "../../draft/run-intent";
import { AgainPicker } from "./again-picker";
import { TemplatePicker } from "./template-picker";

/** A titleless block drawn as a StepSection's card: the same radius, border and padding. */
export const QUESTION_BLOCK = "rounded-xl border border-border bg-card p-4";

const SOURCES = [
  ["scratch", NR_SHAPE.SOURCE_SCRATCH],
  ["template", NR_SHAPE.SOURCE_TEMPLATE],
  ["again", NR_SHAPE.SOURCE_AGAIN],
] as const;

/**
 * Make's "Start from" block (App.tsx:1187-1242): a three-way segmented control with an
 * Optional badge, then the chosen source's picker. The pressed button is the draft's `guided.startFrom`.
 */
export function StartFrom({ startFrom, templateId, againRunId, onSource, onPickTemplate, onPickAgain }: {
  startFrom: GuidedFacts["startFrom"];
  templateId?: string;
  againRunId?: string;
  onSource: (source: GuidedFacts["startFrom"]) => void;
  onPickTemplate: (template: TemplateSummary) => void;
  onPickAgain: (intent: LaunchIntentSummary) => void;
}) {
  const labelId = React.useId();
  return (
    <StepSection title={NR_SHAPE.START_FROM} titleId={labelId} titleAs="div" marker={NR_SHARED.OPTIONAL}>
      <div role="group" aria-labelledby={labelId} className="flex rounded-[9px] border border-border bg-muted p-[3px] max-[350px]:flex-col">
        {SOURCES.map(([value, label]) => (
          <button
            key={value}
            type="button"
            aria-pressed={startFrom === value}
            onClick={() => onSource(value)}
            className={cn(
              "flex-1 rounded-md px-3 py-[9px] text-xs transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring",
              startFrom === value ? "bg-card text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {startFrom === "template" && <TemplatePicker selectedId={templateId} onPick={onPickTemplate} />}
      {startFrom === "again" && <AgainPicker selectedRunId={againRunId} onPick={onPickAgain} />}
    </StepSection>
  );
}
