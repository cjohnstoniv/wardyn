/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Start a run like this one" for a run that kept no launch intent: the prefill
// the run row and its `run.create` audit event can honestly give. It rides
// router state to New Run, where prefillSeed reads it. A run that kept its
// setup opens through `?again=` instead and never reaches this file.

import type { AuditEvent, ConfinementClass } from "../../../../lib/types";
import { createRequestFromAudit } from "../../../../lib/api/audit";
import type { PrefillState } from "./prefill-seed";

/** The router state New Run reads. Structured-cloneable: it lands in history. */
export type RunPrefill = {
  state: PrefillState;
  /** The source run's policy was written inline, and an inline policy is never
   *  persisted, so the clone cannot carry the policy that governed the run it
   *  copies. New Run states that ceiling instead of silently using the default. */
  inlinePolicy: boolean;
  /** The Runs composer rides this same channel but is not a clone; "composer"
   *  suppresses the clone banner, which would be false copy for it. */
  source?: "composer";
};

/** The request-scoped half of a run, as read back off its `run.create` audit row. */
export type RunCreateRequestFacts = {
  task_mode?: string;
  interactive_start?: string;
  seed_auto_tools?: boolean;
  tool_approvals?: string;
  inline_policy?: boolean;
};

// Everything the run row durably holds that a clone can use.
type ClonableRun = {
  agent: string;
  task: string;
  policy_id?: string;
  confinement_class: ConfinementClass;
  interactive?: boolean;
  workspace_ids?: string[];
};

/**
 * The prefill for "start a run like this one", from the two durable sources a
 * finished run leaves behind: the row, and its `run.create` audit event. The
 * row carries no task_mode, interactive_start or tool_approvals (the event
 * stamps them because none is stored), so a clone off the row alone would
 * quietly turn a held run into an unsupervised one.
 *
 * What no clone carries, by decision: the inline policy body (never
 * persisted), credential grants (minted per run), approvals, egress lists
 * and first-use posture (they live in the policy), the drive and per-workspace
 * overrides (request-scoped, not on the row), the image (the resolved sandbox
 * image, not the request: carrying it would ask to wrap a run's own wrapper),
 * and title and description. Create re-clamps everything, so a member cloning
 * an admin's run is narrowed honestly at launch.
 */
export function runPrefill(run: ClonableRun, created: RunCreateRequestFacts = {}): RunPrefill {
  return {
    inlinePolicy: created.inline_policy === true,
    state: {
      // task_mode is the audit row's, and the only record that this run was a shell command.
      runType: created.task_mode === "exec" ? "command" : "agent",
      agent: run.agent,
      // The row holds one text; the audit row says which field it was typed in.
      ...(created.task_mode === "exec"
        ? { command: run.task }
        : run.interactive && created.interactive_start === "shell"
          ? { startupCommand: run.task }
          : { task: run.task }),
      mode: run.interactive ? "interactive" : "batch",
      interactiveStart: created.interactive_start === "shell" ? "shell" : "agent",
      toolApprovals: created.tool_approvals === "hold" ? "hold" : "auto",
      confinementClass: run.confinement_class,
      // undefined for an inline-policy run, which is what makes the ceiling note necessary.
      selectedPolicyId: run.policy_id,
      workspaces: (run.workspace_ids ?? []).map((id) => ({ workspaceId: id })),
    },
  };
}

/** Shown when a clone is refused because the run's launch settings could not be read. */
export const CLONE_UNREADABLE =
  "This run's launch settings couldn't be read — its clone would start from defaults, so it was not opened.";

/**
 * The one door both clone affordances open through. `null` when the run's own
 * `run.create` row is missing (an older run, a pruned trail, or a non-owner's
 * empty 200), so no door launches a clone with defaults standing in for the
 * request-scoped half. The caller toasts CLONE_UNREADABLE and does not navigate.
 */
export function cloneFromAudit(run: ClonableRun, events: AuditEvent[]): RunPrefill | null {
  if (!events.some((e) => e.action === "run.create")) return null;
  return runPrefill(run, createRequestFromAudit(events));
}
