/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Approvals needed: how many people must approve each kind of governance change. Every string the
// tab shows is here. The strings are the approved wording, byte for byte. The
// server's own sentence on a refused save is shown as written and is not copied here.
import { CHANGES } from "./governance-copy";

export const APPROVALS_NEEDED = {
  // The tab's title is the label a held edit carries in the queue, so the two cannot drift.
  TITLE: CHANGES.KIND.governance_approval_counts,
  LEAD: "How many people must approve each kind of governance change. The proposer never counts, and the admin token is not a person.",
  MIN_LABEL: "Deploy-time minimum",
  MIN_HINT: "Set when Wardyn was deployed. This page can't go below it.",
  // With no minimum set the table is read-only. {env} is filled with the variable's name, in mono, where the
  // sentence is shown.
  MIN_ZERO:
    "Set {env} to require more than one approval. Until then, new changes need one approval. Pending changes keep their existing approval requirements.",
  ENV_SLOT: "{env}",

  COL_KIND: "Kind",
  COL_APPROVALS: CHANGES.COL_APPROVALS,
  COL_SECURITY: "At least one security admin",
  COL_WHO: "Who can approve",
  COL_NOW: "Can approve now",
  // The accessible name of a row's number box and switch: the row's kind, then its column.
  ROW_FIELD: (kind: string, column: string) => `${kind} · ${column}`,

  EVERY_OTHER_KIND: "Every other kind",
  DEFAULT_COUNT: "min",

  WHO: {
    security: "Security admins and super admins",
    super_admin: "Super admins",
    security_or_unit_admin: "Security admins, super admins, and unit admins of the unit",
  } as Record<string, string>,

  IN_SMALLEST_UNIT: (n: number, unit: string) => `${n} in the smallest unit, ${unit}`,
  EXACT: (n: number) =>
    `Met only when the proposer isn't one of these ${n}. If one of them proposes, only ${n - 1} others can approve, so the change can't pass.`,
  SHORT: (n: number, kind: string, need: number) => `Can't be met: ${n} people can approve ${kind} changes, fewer than ${need}.`,
  CANT_CONFIRM: "Can't confirm",
  // Why a pool can't be confirmed.
  CANT_CONFIRM_UNCONFIRMED:
    "Can't confirm: Wardyn can't read the role of some people who could approve. They are counted once they sign in again.",
  CANT_CONFIRM_TOO_MANY: "Can't confirm: too many people could approve for Wardyn to count them all.",
  // Under the table.
  NEVER_SIGNED_IN: "People who have never signed in aren't counted until they do.",

  NOTE_HELD: (highest: number) => `Editing these rules is itself a change. It is held for ${highest} approvals, the highest count in force.`,
  NOTE_REQUESTS: "Run holds and requests (elevation, image) keep one decider.",
  SAVE: "Save",

  // A refused save. The server's sentence goes under the title; a 422 adds the second sentence.
  REFUSED_TITLE: "Couldn't save the approvals needed",
  REFUSED_NEXT:
    "Lower the count, or give another person the right to approve. A change that stalls can still be applied with the admin token, recorded as a bypass.",
  // The counts could not be read.
  FETCH_FAILED_TITLE: "Couldn't load the approvals needed",
  FETCH_FAILED_BODY: "Something went wrong reaching the server. Nothing here has changed.",
} as const;
