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

/** A run a service identity created: created_by is "service:<name>", and the sponsor enters it (service_attach). */
export const isServiceRun = (run: RunEntryFacts): boolean => !!run.created_by?.startsWith("service:");

export function mayEnterRun(run: RunEntryFacts, principal: string | null | undefined, operator: boolean): boolean {
  return (!!principal && run.created_by === principal) || (operator && !!run.operator_owned);
}

/**
 * The same rule for a page that renders before it knows who is looking. When
 * /me failed, `operator` fails open as true and the principal is a placeholder,
 * so mayEnterRun reads false for the run's REAL owner; the page must not take
 * their terminal on that guess. An unresolved identity defers to the server,
 * whose ticket mint is the enforcement point (R4-F110). A service identity's
 * run is the same case: its sponsor may enter it under a feature grant the
 * console cannot see, so the mint decides there too.
 */
export function mayEnterRunOrUnknown(
  run: RunEntryFacts,
  principal: string | null | undefined,
  operator: boolean,
  resolved: boolean,
): boolean {
  return !resolved || isServiceRun(run) || mayEnterRun(run, principal, operator);
}

/** The server's refusal reason for interactive entry to a run the caller does not own. */
export const RUN_OWNER_ONLY_REASON = "run_owner_only";

/** The server's refusal reason when a run already has the most live terminal sessions it allows. */
export const ATTACH_CAP_REASON = "attach_session_cap_reached";

/** The sponsor's entry arm only: SSH never takes it, the server answers a sponsor's SSH with run_owner_only. */
export const sponsorOnlyEntry = (run: RunEntryFacts, principal: string | null | undefined, operator: boolean): boolean =>
  isServiceRun(run) && !mayEnterRun(run, principal, operator);

/** The sponsor arm's refusal (a 404, so a foreign run reads the same): the feature grant or the sponsorship is missing. */
export const RUN_NOT_FOUND_REASON = "run_not_found";
export const SERVICE_ATTACH_LINE =
  "This run's terminal isn't available to you. If you sponsor its service identity, ask an admin to turn on the service_attach feature for you.";

/** What the console says for a refused entry, whatever surface it surfaces on. */
export const RUN_OWNER_ONLY = "Only the person who started this run can open it interactively.";

export const SERVICE_ENTRY_LINE = "Only the people who sponsor this service identity can open this run's terminal and apps.";

/** The pane line for someone who can see a person's run but not enter it. */
export const runEntryOwnerLine = (owner: string): string => `Only ${owner} can open this run's terminal, apps and SSH.`;

/**
 * The line for a viewer who may not enter this run. A run no person owns keeps
 * "Requires the admin role." (still true there); a person's run names the person.
 */
export function runEntryRefusalLine(run: RunEntryFacts): string {
  if (run.operator_owned || !run.created_by) return OPERATOR_ONLY_REASON;
  // A service identity is no person who could open it: its sponsors can.
  return isServiceRun(run) ? SERVICE_ENTRY_LINE : runEntryOwnerLine(run.created_by);
}

/**
 * An error from an entry call, in console words. The server's lowercase wire
 * sentence for `run_owner_only` never reaches the screen: it is mapped from the
 * reason, not matched on text.
 */
export function entryErrorMessage(e: unknown, fallback: (e: unknown) => string): string {
  if (e instanceof HttpError && e.reason === RUN_OWNER_ONLY_REASON) return RUN_OWNER_ONLY;
  if (e instanceof HttpError && e.status === 404 && e.reason === RUN_NOT_FOUND_REASON) return SERVICE_ATTACH_LINE;
  return fallback(e);
}
