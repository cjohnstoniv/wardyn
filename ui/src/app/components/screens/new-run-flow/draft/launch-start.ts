/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Whether this draft starts later, and what that start reads like. Only an Autonomous agent run can be
// scheduled, and only while the setup status carries features.scheduled_runs; anything else starts now. A Later
// start the draft still holds while the switch is off, or not known, is never started now unasked: the Run step,
// Review and the Name dialog say why it is held (startHeldText), and the dialog holds Launch until the person
// chooses Run now.

import { startProblem, type StartChoice, type StartProblem } from "../../../../lib/schedule";
import { SCHEDULE_REFUSAL } from "../../../../lib/schedule-refusals";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import type { NewRunDraft } from "./run-intent";

export interface LaunchStart {
  choice: StartChoice;
  problem: StartProblem | null;
}

type Draft = Pick<NewRunDraft, "intent" | "schedule">;

// The Later start an Autonomous agent run's draft holds, whatever the switch says.
const laterStart = ({ intent, schedule }: Draft): StartChoice | undefined =>
  schedule && intent.shape === "agent_task" && intent.experience === "background" ? schedule : undefined;

/** The start the draft asks for, or undefined when it starts now. `on` is scheduledRuns(setup status). */
export function launchStart(draft: Draft, on: boolean | undefined, now: number): LaunchStart | undefined {
  const choice = on === true ? laterStart(draft) : undefined;
  return choice && { choice, problem: startProblem(choice, now) };
}

/** Why a Later start is held: the switch is off, or the server has not said whether it is on. */
export type StartHeld = "off" | "unknown";

/** Why the Later start `later` is held, or null when there is none or the switch keeps it. `on` is scheduledRuns(setup status). */
export const heldStart = (later: StartChoice | undefined, on: boolean | undefined): StartHeld | null =>
  later === undefined || on === true ? null : on === false ? "off" : "unknown";

/** Why the draft's Later start is held, or null. */
export const startHeld = (draft: Draft, on: boolean | undefined): StartHeld | null => heldStart(laterStart(draft), on);

/** What a held start says wherever the draft shows it: the server's own off sentence, or that the switch is not known. */
export const startHeldText = (held: StartHeld): string => (held === "off" ? SCHEDULE_REFUSAL.OFF() : NR_RUN.START_HELD_UNKNOWN);
