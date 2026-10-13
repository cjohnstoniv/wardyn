/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for the Save as dialog, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_SAVE = {
  TITLE_TEMPLATE: "Save as template",
  TITLE_POLICY: "Save as policy",
  INTRO: (kind: string | number) =>
    `Saving changes the ${kind}, not this run draft. A run title is never stored.`,
  NAME_LABEL: "Name",
  NAME_PLACEHOLDER_TEMPLATE: "e.g. Frontend investigation",
  NAME_PLACEHOLDER_POLICY: "e.g. Package access",
  AVAILABLE_LABEL: "Available to",
  SCOPE_ME: "Only me",
  SCOPE_ORG: "Organisation",
  SCOPE_GROUP: (label: string | number) => `${label} group`,
  /** pending W-20: the recommended text until the owner answers. */
  HELP_NO_GRANTS:
    "Only you can use it. Organisation admins and group template managers publish shared templates.",
  /** pending W-20: the recommended text until the owner answers. */
  HELP_MANAGER: "You can also publish to the groups you manage.",
  INCLUDE_LEGEND: "Include",
  INCLUDE_ALL: "Everything in this draft",
  INCLUDE_SUBSET: "Choose a subset",
  /** pending W-21: the recommended text until the owner answers. */
  SUBSET_NETWORK: "Network",
  /** pending W-21: the recommended text until the owner answers. */
  SUBSET_CREDENTIALS: "Credentials",
  /** pending W-21: the recommended text until the owner answers. */
  SUBSET_ACCESS_RULES: "Access rules",
  /** pending W-22: the recommended text until the owner answers. */
  OMITTED:
    "Omitted values inherit defaults or are asked for when it's used. Explicit false, zero and empty values are kept.",
  /** pending W-25: the recommended text until the owner answers. */
  SHARED_DISCLOSURE:
    "A template keeps the names of secrets, workspaces and drives, never their values. People it is shared with can see those names. Each person's own access is checked when they use it, and anything they lack shows as Needs setup.",
  CANCEL_SAVE: "Cancel",
  CONFIRM_TEMPLATE: "Save template",
  CONFIRM_POLICY: "Save policy",
  SAVED: (kind: string | number) => `${kind} saved without changing the draft.`,
} as const;
