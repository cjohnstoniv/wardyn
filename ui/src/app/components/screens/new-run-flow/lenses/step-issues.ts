/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { FieldHome } from "../draft/field-homes";
import type { RunIntent } from "../draft/run-intent";

export type NewRunStep = "shape" | "run" | "harnesses" | "files" | "tools";
export const NEW_RUN_STEPS: readonly NewRunStep[] = ["shape", "run", "harnesses", "files", "tools"];

export type StepIssueCode =
  | "shape_required"
  | "harness_required"
  | "task_required"
  | "agent_task_required"
  | "command_required"
  | "pool_required"
  | "main_drive_size_required"
  | "files_choice_required"
  | "starts_here_required"
  | "doc_invalid";

/**
 * A local completeness issue. The sentence for each code is a canon string in
 * the copy modules; the issue carries only where the fix lives and which
 * control to focus.
 */
export interface StepIssue {
  home: FieldHome;
  code: StepIssueCode;
  /** DOM id of the control a failed Continue focuses. */
  focus: string;
}

/** The control ids step bodies give the fields an issue points at. */
export const ISSUE_FOCUS: Record<StepIssueCode, string> = {
  shape_required: "nr-workload",
  harness_required: "nr-harness",
  task_required: "nr-task",
  agent_task_required: "nr-task",
  command_required: "nr-command",
  pool_required: "nr-pool",
  main_drive_size_required: "nr-main-drive-size",
  files_choice_required: "nr-files-choice",
  starts_here_required: "nr-starts-here",
  doc_invalid: "nr-document",
};

/** What a validator needs beyond the intent. Unknown facts never block: the server decides. */
export interface IssueContext {
  /** False only when the Run step's pool list is loaded and offers no default. */
  poolDefault?: boolean;
}

export const issue = (home: FieldHome, code: StepIssueCode): StepIssue => ({ home, code, focus: ISSUE_FOCUS[code] });

export type StepValidator = (intent: RunIntent, ctx: IssueContext) => StepIssue[];
