/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Whether this draft starts later, and what that start reads like. Only an Autonomous agent run can be
// scheduled, and only while the setup status carries features.scheduled_runs; anything else starts now,
// whatever the draft last held.

import { startProblem, type StartChoice, type StartProblem } from "../../../../lib/schedule";
import type { NewRunDraft } from "./run-intent";

export interface LaunchStart {
  choice: StartChoice;
  problem: StartProblem | null;
}

/**
 * The start the draft asks for, or undefined when it starts now. `on` is scheduledRunsOn(setup status);
 * `waitSec` is the preflight's max_wait_sec for this run, absent until the server has answered.
 */
export function launchStart(draft: Pick<NewRunDraft, "intent" | "schedule">, on: boolean, now: number, waitSec?: number): LaunchStart | undefined {
  const { intent, schedule } = draft;
  if (!on || !schedule || intent.shape !== "agent_task" || intent.experience !== "background") return undefined;
  return { choice: schedule, problem: startProblem(schedule, now, waitSec) };
}
