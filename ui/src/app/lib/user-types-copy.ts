/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User types screen copy (0.8, UT-7a). EXPLAIN is the "What this type gets"
// grid's canon and USER_TYPES is the rest of the screen; both are frozen in
// docs/design/user-types-canon.md (user-types-copy.test.ts parses them back).
// EXPLAIN started from the owner-approved user types packet A (2026-09-23);
// packet UT-G (2026-09-27) approved the eight gaps packet A left open — the
// families packet A didn't draw, the `*` row on the other seven kinds, a git
// provider's label, more than one audience after "only", the legend, the Add
// button, and USER_TYPES itself, which packet A never drew at all.
//
// The Ceiling and run limits section reuses GOVERNANCE's own limit labels
// (governance/limit-chips.tsx) rather than re-wording them — a profile's
// "Deny exec runs" reads the same whether it's found through Governance or
// through the type that carries it.
//
// TITLE reads USER_TYPES_NAV_TITLE (nav-copy.ts) — the same constant
// app-shell.tsx's eager sidebar uses — so the nav label and the screen
// heading can't drift apart (governance-copy.ts's own pattern).
import { USER_TYPES_NAV_TITLE } from "./nav-copy";

export const USER_TYPES = {
  TITLE: USER_TYPES_NAV_TITLE,
  LEAD: "A kind of person your organisation defines, like Portfolio manager or Contractor. Resources name it in \"Available to\", and its ceiling and run limits come from the governance profile assigned to it.",

  // ---- list ----
  COL_NAME: "Name",
  COL_DESCRIPTION: "Description",
  COL_PRIORITY: "Priority",
  COL_UPDATED: "Updated",
  BUILT_IN_BADGE: "Built in",
  NEW_CTA: "New type",
  EDIT: "Edit",
  DELETE: "Delete",
  EMPTY_TITLE: "No custom types yet",
  EMPTY_BODY: "Everyone signs in as Standard user until you add one.",
  FETCH_FAILED_TITLE: "Couldn't load user types",
  FETCH_FAILED_BODY: "The list below may be stale or empty. Nothing was changed.",

  // ---- editor ----
  ADD_TITLE: "New type",
  EDIT_TITLE: (name: string) => `Edit ${name}`,
  FIELD_NAME: "Name",
  FIELD_DESCRIPTION: "Description",
  DESCRIPTION_HINT: "Shown to a person of this type on their Getting started page.",
  FIELD_PRIORITY: "Priority",
  PRIORITY_HINT: "Breaks a sign-in tie between two custom types — higher wins. Standard user never takes part in a tie.",
  FIELD_ID: "Id",
  ID_HINT: "Role mappings and grants use this to refer to the type. It's made from the name unless you set it, and it can't change once saved.",
  SAVE: "Save",
  CANCEL: "Cancel",

  // ---- delete ----
  // The "? " split point (governance/display.tsx's question()) is deliberate:
  // the head is the dialog title, the tail its consequence line.
  DELETE_CONFIRM: (name: string) =>
    `Delete ${name}? If a role mapping, grant, assignment, drive grant or run still names it, the delete is refused and nothing changes.`,
  DELETE_BUILTIN: "Standard user can't be deleted — it's the type everyone signs in as when no other type matches.",
  DELETE_RESTRICT_TITLE: "Still in use",

  // ---- Ceiling and run limits (design §2.2, read from the assigned profile) ----
  CEILING_TITLE: "Ceiling and run limits",
  CEILING_LEAD: "Read-only here — assign or change the profile from Governance.",
  CEILING_NONE: "No governance profile is assigned to this type. Runs fall back to the deployment ceiling.",
  CEILING_PROFILE: (name: string) => `Uses the ${name} governance profile.`,
  OPEN_GOVERNANCE: "Open in Governance",

  // ---- What this type gets: its load failure (the rest is EXPLAIN below) ----
  EXPLAIN_LOAD_FAILED: "Couldn't load what this type gets.",
};

// "What this type gets" (design §2.6), from packet A. The audience after ONLY
// is a type's name or AVAILABILITY's group/person chip text; the family
// headings are KIND's labels (permissions-copy.ts).
export const EXPLAIN = {
  TITLE: "What this type gets",
  STATE: {
    everyone: "Everyone",
    this_type: "This type",
    blocked: "Blocked",
    admins_only: "Admins only",
    not_available: "Not available",
  } as const,
  // Set after the value as " · only Developer".
  ONLY: (who: string) => `only ${who}`,
  // G-5: two or three audiences name each one, joined with "and" before the
  // last (c empty means exactly two: "a and b"). Four or more names the first
  // two and counts the rest — the full list stays a hover (and the row's
  // accessible name) away, unelided.
  WHO_LIST: (a: string, b: string, c: string) => (c ? `${a}, ${b} and ${c}` : `${a} and ${b}`),
  WHO_MORE: (a: string, b: string, n: string) => `${a}, ${b} and ${n} more`,
  // The wall note under a family with a block: WALL_HEAD bold, then WALL_BODY.
  WALL_HEAD: "A block here is a wall.",
  WALL_BODY: "It blocks everyone of this type, and an allow for a person or a group does not override it.",
  REMOVE: "Remove",
  // G-7: opens Permissions' own "Add a grant" dialog, Who and Capability fixed.
  ADD: "Add",
  // G-6: the grid's one true legend line. Packet A's own second footer
  // (FOOTER_OTHER_SIDE) is true only now that G-7 gives the grid an add.
  LEGEND: "Green: this type gets it. Red: blocked for this type. Grey: not this type's own — the chip says why.",
  FOOTER_OTHER_SIDE:
    "Same rows, written from the other side: adding a row here is the same as ticking this type on the resource.",
  // Values the packet names in words. Every other value shows as written.
  ALL_WORKSPACES: "All org workspaces",
  ALL_IMAGES: "Images",
  SSH_KEYS: "SSH keys",
  API_TOKENS: "API tokens",
  SSH_AND_TOKENS: "SSH keys · API tokens",
  // G-3: the `*` row on the seven kinds packet A didn't name.
  ALL_EGRESS_HOSTS: "All egress hosts",
  ALL_SECRETS: "All secrets",
  ALL_AGENTS: "All agents",
  ALL_GIT_PROVIDERS: "All git providers",
  ALL_MODEL_PROVIDERS: "All model providers",
  ALL_POLICIES: "All stored policies",
};
