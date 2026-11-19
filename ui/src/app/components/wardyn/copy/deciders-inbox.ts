/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Approvals screen's words for who decides a hold under a unit's deciders rule: the Decided by line, the
// Requested by row, the scope cap in the Approve dialog, the Waiting on you states, the basis line on decided
// rows and the three refusal toasts. The tab's own label is APPROVALS_TAB.WAITING (copy/approvals.ts).
export const DECIDERS_INBOX = {
  DECIDED_BY_LABEL: "Decided by:",
  OWNER_SELF: "you",
  OWNER_OTHER: "the run's owner",
  UNIT_ADMINS: "admins of this unit",
  GROUP: (name: string) => `the ${name} group`,
  SECURITY: "a security admin",
  SECOND_PERSON: "A second person decides: never someone who started or wrote the run, security admins too.",
  SCOPE_CAP: (label: string) => `This unit's rule allows nothing wider than ${label}.`,
  HELP: "A profile is a governance unit.",
  REQUESTED_BY: (who: string, run: string) => ({ before: "Requested by ", who, middle: " · run ", run }),
  CANT_OPEN_RUN: "You can't open this run.",
  SOMEONE_ELSE: "Someone else decides this",
  EMPTY_TITLE: "Nothing is waiting on you",
  EMPTY_BODY: "Requests you can decide appear here, including ones from other people's runs.",
  EMPTY_ACTION: "See decided",
  TRUNCATED: "Showing the 500 newest requests you can decide. Older ones aren't listed here.",
  BASIS_OWNER: "Decided as the run's owner",
  BASIS_UNIT_ADMIN_OF: (name: string) => `Decided as an admin of ${name}`,
  BASIS_UNIT_ADMIN: "Decided as a unit admin",
  BASIS_GROUP: (name: string) => `Decided as a member of the ${name} group`,
  BASIS_SECURITY: "Decided as a security admin",
  BASIS_POLICY: "Decided by an approval rule, not a person",
  BASIS_ADMIN_TOKEN: "Decided with the admin token",
  TOAST_NOT_A_DECIDER: "Someone else decides this request",
  TOAST_SCOPE_ABOVE_RULE: "That choice is wider than this unit allows",
  TOAST_UNREADABLE: "Couldn't check who decides this",
  TOAST_UNREADABLE_BODY: "Nothing was decided. Try again in a moment.",
} as const;
