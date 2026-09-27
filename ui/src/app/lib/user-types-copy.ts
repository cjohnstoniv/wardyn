/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User types screen copy (0.8, UT-7a). EXPLAIN is the "What this type gets"
// grid's canon, copied from the owner-approved user types packet A and frozen
// in docs/design/user-types-canon.md (user-types-copy.test.ts parses it back).
// USER_TYPES is the rest of the screen, which packet A does not draw: it is
// not frozen and waits on an owner mock round (the canon doc lists it).
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
  LEAD: "An org-defined kind of person — Portfolio manager, Contractor. A type carries no controls of its own; every admin-set resource names it through \"Available to\", and its ceiling and run limits come from the governance profile assigned to it.",

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
  ID_HINT: "The immutable slug role mappings and grants refer to. Derived from the name; set it yourself to pin it.",
  SAVE: "Save",
  CANCEL: "Cancel",

  // ---- delete ----
  // The "? " split point (governance/display.tsx's question()) is deliberate:
  // the head is the dialog title, the tail its consequence line.
  DELETE_CONFIRM: (name: string) => `Delete ${name}? Anyone still mapped to it, or a grant, assignment, drive grant or run that still names it, refuses the delete instead.`,
  DELETE_BUILTIN: "Standard user can't be removed — it's the sign-in floor every deployment falls back to.",
  DELETE_RESTRICT_TITLE: "Still in use",

  // ---- Ceiling and run limits (design §2.2, read from the assigned profile) ----
  CEILING_TITLE: "Ceiling and run limits",
  CEILING_LEAD: "Read-only here — assign or change the profile from Governance.",
  CEILING_NONE: "No governance profile is assigned to this type. Runs fall back to the deployment ceiling.",
  CEILING_PROFILE: (name: string) => `Bound to the "${name}" profile.`,
  OPEN_GOVERNANCE: "Open in Governance",

  // ---- What this type gets: its load failure (the rest is EXPLAIN below) ----
  EXPLAIN_LOAD_FAILED: "Couldn't compute this grid.",
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
  // The wall note under a family with a block: WALL_HEAD bold, then WALL_BODY.
  WALL_HEAD: "A block here is a wall.",
  WALL_BODY: "It blocks everyone of this type, and an allow for a person or a group does not override it.",
  REMOVE: "Remove",
  // Values the packet names in words. Every other value shows as written.
  ALL_WORKSPACES: "All org workspaces",
  ALL_IMAGES: "Images",
  SSH_KEYS: "SSH keys",
  API_TOKENS: "API tokens",
  SSH_AND_TOKENS: "SSH keys · API tokens",
};
