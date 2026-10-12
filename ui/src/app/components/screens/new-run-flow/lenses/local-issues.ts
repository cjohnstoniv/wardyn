/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { DocBuffer, RunIntent } from "../draft/run-intent";
import { filesIssues } from "./files-lens";
import { harnessIssues } from "./harness-lens";
import { runIssues } from "./run-lens";
import { shapeIssues } from "./shape-lens";
import { issue, NEW_RUN_STEPS, type IssueContext, type NewRunStep, type StepIssue, type StepValidator } from "./step-issues";
import { toolsIssues } from "./tools-lens";

export const STEP_VALIDATORS: Record<NewRunStep, StepValidator> = {
  shape: shapeIssues,
  run: runIssues,
  harnesses: harnessIssues,
  files: filesIssues,
  tools: toolsIssues,
};

export const stepIssues = (step: NewRunStep, intent: RunIntent, ctx: IssueContext = {}): StepIssue[] =>
  STEP_VALIDATORS[step](intent, ctx);

/** Every local issue in step order, then an editor buffer the server has not accepted. */
export function localIssues(intent: RunIntent, ctx: IssueContext = {}, doc?: DocBuffer): StepIssue[] {
  const out = NEW_RUN_STEPS.flatMap((step) => stepIssues(step, intent, ctx));
  if (doc && !doc.validated) out.push(issue("document", "doc_invalid"));
  return out;
}

/** The first step with a local issue, or undefined when every step is complete. */
export const firstInvalidStep = (intent: RunIntent, ctx: IssueContext = {}): NewRunStep | undefined =>
  NEW_RUN_STEPS.find((step) => stepIssues(step, intent, ctx).length > 0);
