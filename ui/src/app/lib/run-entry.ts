/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Who may OPEN a run interactively (its terminal, apps and SSH). One question,
// asked at every attach site so the page never promises what the server then
// refuses (#1476): the run's own person, or an admin on a run no person owns
// (an operator-owned service or local run). An admin on another person's run
// keeps Kill, Approve and Policy, and loses entry.
//
// `operator_owned` rides AgentRun (lib/types/runs.ts, #1476); typed structurally
// here so this file asks only for the two facts it reads.

import { HttpError } from "./api/core";
import { OPERATOR_ONLY_REASON } from "../components/wardyn/copy";

export type RunEntryFacts = { created_by?: string; operator_owned?: boolean };

export function mayEnterRun(run: RunEntryFacts, principal: string | null | undefined, operator: boolean): boolean {
  return (!!principal && run.created_by === principal) || (operator && !!run.operator_owned);
}

/** The server's refusal reason for interactive entry to a run the caller does not own. */
export const RUN_OWNER_ONLY_REASON = "run_owner_only";

/** What the console says for a refused entry, whatever surface it surfaces on. */
export const RUN_OWNER_ONLY = "Only the person who started this run can open it interactively.";

/** The pane line for someone who can see a person's run but not enter it. */
export const runEntryOwnerLine = (owner: string): string => `Only ${owner} can open this run's terminal, apps and SSH.`;

/**
 * The line for a viewer who may not enter this run. A run no person owns keeps
 * "Requires the admin role." (still true there); a person's run names the person.
 */
export function runEntryRefusalLine(run: RunEntryFacts): string {
  return run.operator_owned || !run.created_by ? OPERATOR_ONLY_REASON : runEntryOwnerLine(run.created_by);
}

/**
 * An error from an entry call, in console words. The server's lowercase wire
 * sentence for `run_owner_only` never reaches the screen: it is mapped from the
 * reason, not matched on text.
 */
export function entryErrorMessage(e: unknown, fallback: (e: unknown) => string): string {
  return e instanceof HttpError && e.reason === RUN_OWNER_ONLY_REASON ? RUN_OWNER_ONLY : fallback(e);
}
