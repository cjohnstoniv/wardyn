/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 1, Shape: where the run starts from, and what it is. Choosing a saved
// template or an earlier run opens Review on that setup; choosing a workload
// writes the shape and the carriers that say it.

import * as React from "react";
import { useNavigate } from "react-router-dom";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunShape } from "../../../../../lib/types/runs";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { newRunHref } from "../../draft/new-run-url";
import { chooseShape } from "../../lenses/shape-lens";
import { stepIssues } from "../../lenses/local-issues";
import { flowSteps, StepShell } from "../step-shell";
import { StartFrom } from "./start-from";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const OPEN_REVIEW = { screen: "review", view: "pretty" } as const;

export function ShapeStep({ go, startFromScratch }: NewRunScreenProps) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const navigate = useNavigate();
  const labelId = React.useId();

  // A template or run in the draft is dropped, not edited: scratch means a blank draft here, on this step.
  const chooseSource = (startFrom: typeof draft.guided.startFrom) => {
    if (startFrom === "scratch" && (draft.origin.kind === "template" || draft.origin.kind === "again")) {
      startFromScratch();
      void navigate(newRunHref({ screen: "step", step: "shape" }), { replace: true });
      return;
    }
    dispatch({ type: "guided", patch: { startFrom } });
  };

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(1)}
      title={NR_SHAPE.TITLE}
      lede={NR_SHAPE.LEDE}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "shape") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "start" }) }}
      primary={{ label: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_RUN), onActivate: () => go({ screen: "step", step: "run" }) }}
      issues={stepIssues("shape", draft.intent)}
    >
      <StartFrom
        startFrom={draft.guided.startFrom}
        origin={draft.origin}
        onSource={chooseSource}
        onPickTemplate={(t) => void navigate(newRunHref(OPEN_REVIEW, { template: { id: t.id, revision: t.revision } }))}
        onPickAgain={(i) => void navigate(newRunHref(OPEN_REVIEW, { again: i.run_id }))}
      />
      <div id="nr-workload" tabIndex={-1} className="flex flex-col gap-3 outline-none">
        <span id={labelId} className="sr-only">{NR_SHAPE.TITLE}</span>
        <RadioCardGroup
          name="nr-workload-shape"
          aria-labelledby={labelId}
          layout="row"
          value={draft.intent.shape ?? ""}
          onValueChange={(shape) => dispatch({ type: "edit", apply: (intent) => chooseShape(intent, shape as RunShape) })}
        >
          <RadioCard value="agent_task" title={NR_SHAPE.AGENT_TITLE} description={NR_SHAPE.AGENT_BODY} meta={NR_SHAPE.AGENT_SUB} />
          <RadioCard value="environment" title={NR_SHAPE.ENV_TITLE} description={NR_SHAPE.ENV_BODY} meta={NR_SHAPE.ENV_SUB} />
          <RadioCard value="application" title={NR_SHAPE.APP_TITLE} description={NR_SHAPE.APP_BODY} meta={NR_SHAPE.APP_SUB} />
        </RadioCardGroup>
      </div>
    </StepShell>
  );
}
