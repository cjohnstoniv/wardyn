/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 1, Shape, as Make lays it out (App.tsx:1162-1296). From scratch: a workload
// button writes the shape and the carriers that say it, then opens Run. A saved
// template or an earlier run hides the workloads; Continue opens Review on the one shown.

import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunShape } from "../../../../../lib/types/runs";
import type { TemplateSummary } from "../../../../../lib/types/templates";
import type { LaunchIntentSummary } from "../../../../../lib/new-run-fixtures/scenarios/templates";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { newRunHref } from "../../draft/new-run-url";
import { chooseShape } from "../../lenses/shape-lens";
import { flowSteps, StepShell } from "../step-shell";
import { QUESTION_BLOCK, StartFrom } from "./start-from";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const OPEN_REVIEW = { screen: "review", view: "pretty" } as const;

const WORKLOADS: { shape: RunShape; title: string; body: string; sub: string }[] = [
  { shape: "agent_task", title: NR_SHAPE.AGENT_TITLE, body: NR_SHAPE.AGENT_BODY, sub: NR_SHAPE.AGENT_SUB },
  { shape: "environment", title: NR_SHAPE.ENV_TITLE, body: NR_SHAPE.ENV_BODY, sub: NR_SHAPE.ENV_SUB },
  { shape: "application", title: NR_SHAPE.APP_TITLE, body: NR_SHAPE.APP_BODY, sub: NR_SHAPE.APP_SUB },
];

type Pick = { kind: "template"; template: TemplateSummary } | { kind: "again"; intent: LaunchIntentSummary };

export function ShapeStep({ go, startFromScratch }: NewRunScreenProps) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const navigate = useNavigate();
  const { origin } = draft;
  const scratch = draft.guided.startFrom === "scratch";
  // The row the open picker shows; Continue opens Review on it.
  const [pick, setPick] = React.useState<Pick>();
  const pickTemplate = React.useCallback((template: TemplateSummary) => setPick({ kind: "template", template }), []);
  const pickAgain = React.useCallback((intent: LaunchIntentSummary) => setPick({ kind: "again", intent }), []);

  // A template or run in the draft is dropped, not edited: scratch means a blank draft here, on this step.
  const chooseSource = (startFrom: typeof draft.guided.startFrom) => {
    setPick(undefined);
    if (startFrom === "scratch" && (origin.kind === "template" || origin.kind === "again")) {
      startFromScratch();
      void navigate(newRunHref({ screen: "step", step: "shape" }), { replace: true });
      return;
    }
    dispatch({ type: "guided", patch: { startFrom } });
  };

  const openPick = () => {
    if (pick?.kind === "template") {
      const { id, revision } = pick.template;
      // The draft's own template keeps the revision its link pinned.
      void navigate(newRunHref(OPEN_REVIEW, { template: { id, revision: origin.kind === "template" && origin.id === id ? origin.revision : revision } }));
    } else if (pick?.kind === "again") {
      void navigate(newRunHref(OPEN_REVIEW, { again: pick.intent.run_id }));
    }
  };

  const chooseWorkload = (shape: RunShape) => {
    dispatch({ type: "edit", apply: (intent) => chooseShape(intent, shape) });
    go({ screen: "step", step: "run" });
  };

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(1)}
      title={NR_SHAPE.TITLE}
      lede={NR_SHAPE.LEDE}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "shape") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "start" }) }}
      primary={scratch ? undefined : { label: NR_SHARED.CONTINUE, onActivate: openPick, disabled: pick === undefined }}
    >
      <StartFrom
        startFrom={draft.guided.startFrom}
        templateId={pick?.kind === "template" ? pick.template.id : origin.kind === "template" ? origin.id : undefined}
        againRunId={pick?.kind === "again" ? pick.intent.run_id : origin.kind === "again" ? origin.runId : undefined}
        onSource={chooseSource}
        onPickTemplate={pickTemplate}
        onPickAgain={pickAgain}
      />
      {scratch && (
        <section id="nr-workload" tabIndex={-1} aria-label={NR_SHAPE.TITLE} className={`${QUESTION_BLOCK} outline-none`}>
          <div className="grid gap-[9px]">
            {WORKLOADS.map((w) => (
              <WorkloadButton key={w.shape} {...w} onChoose={() => chooseWorkload(w.shape)} />
            ))}
          </div>
        </section>
      )}
    </StepShell>
  );
}

/** Make's `.choice-button` (snap-2125Z index.css:142-150): title, body and sub-line, a chevron in its own square. */
function WorkloadButton({ title, body, sub, onChoose }: { title: string; body: string; sub: string; onChoose: () => void }) {
  const id = React.useId();
  return (
    <button
      type="button"
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-body ${id}-sub`}
      onClick={onChoose}
      className="group flex w-full items-center justify-between gap-4 rounded-[9px] border border-border bg-card p-[17px] text-left transition-[border-color,background-color,transform] outline-none hover:translate-x-0.5 hover:border-primary hover:bg-primary/10 focus-visible:ring-[3px] focus-visible:ring-ring motion-reduce:transition-none motion-reduce:hover:translate-x-0"
    >
      <span className="flex min-w-0 flex-col gap-1">
        <strong id={`${id}-title`} className="text-[13px] font-semibold">{title}</strong>
        <span id={`${id}-body`} className="text-xs leading-[1.45] text-muted-foreground">{body}</span>
        <small id={`${id}-sub`} className="text-meta leading-[1.45] text-muted-foreground">{sub}</small>
      </span>
      <span aria-hidden="true" className="grid size-7 shrink-0 place-items-center rounded-[7px] bg-muted text-muted-foreground group-hover:bg-primary group-hover:text-primary-foreground">
        <ChevronRight className="size-4" />
      </span>
    </button>
  );
}
