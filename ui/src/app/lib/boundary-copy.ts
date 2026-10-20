/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary (boards T2 and T4, round 4). Rendered by
// components/screens/policies/boundary-screen.tsx. Every string is byte-exact from the boards except the ones
// marked "not on the boards", which the PR lists under "Copy not on the boards".
export const BOUNDARY = {
  BREADCRUMB_PARENT: "Policies",
  TITLE: "Wardyn boundary",
  LEAD: "The outermost policy Wardyn controls. Every team's walls sit inside it, and every run is bound by it. Your organisation's boundary sits outside it; Wardyn can't see or change that.",
  CHIP_HARD: "Hard limits",
  CHIP_ORG_DRAFT: "As your organisation's boundary allows · Draft",

  // The nested ring strip.
  RINGS_LABEL: "Where the Wardyn boundary sits",
  ORG_BOUNDARY: "Your organisation's boundary",
  NOT_VISIBLE: "Not visible to Wardyn",
  RING_BOUNDARY_SUB: "Set here, by security admins",
  RING_TEAMS: "Team walls",
  RING_TEAMS_SUB: "Each only narrows the one above.",
  RING_RESTRICTIONS: "Restrictions",
  RING_RESTRICTIONS_SUB: "Deny-only. Follow people into any team.",
  RING_RUN: "Run walls",
  RING_RUN_SUB: "The run's own policy.",

  // The mode picker.
  MODE: "Mode",
  MODE_HARD: "Hard limits",
  RECOMMENDED: "Recommended",
  NEW_INSTALLS: "New installs",
  MODE_HARD_BODY: "A short list of what no Wardyn run may ever do. Teams give access inside it; the Default team gives everyone else a working setup.",
  MODE_ORG: "As your organisation's boundary allows",
  NEEDS_SECOND_ADMIN: "Needs a second admin",
  MODE_ORG_BODY: "Wardyn adds no limits of its own at the top. Only the walls below and your organisation's boundary apply.",

  // Hard limits.
  HARD_HEADING: "Hard limits · no team, person or run can go past these",
  BLOCKED_LABEL: "Blocked hosts and ranges",
  BLOCKED_HINT: "No run can reach these, whatever a team allows.",
  /** not on the boards: DESIGN-TREE §8 "UI lanes" names it for an empty boundary. */
  BLOCKED_EMPTY: "No hard limits yet. Add the first.",
  FLOOR_LABEL: "Confinement floor",
  FLOOR_TAIL: (barrier: string) => `and up. No team can allow a run below ${barrier}.`,
  HARD_FOOT: "Anything not listed is left to the teams below. Your organisation's boundary may still block more.",
  ALWAYS_BLOCKED: "Always blocked by Wardyn, whatever this list says: cloud metadata and link-local addresses.",

  // The off state (no_limits).
  ORG_PANEL: "Wardyn adds no limits here. What runs can reach is whatever your organisation's own boundary allows, then what the walls below allow.",
  ORG_PANEL_NOTE: "Not recommended. Saving sends this to another security admin.",

  // Who it binds.
  BINDS_TITLE: "Who the Wardyn boundary binds",
  BINDS_EVERY: "Every run",
  BINDS_EVERY_BODY: "Members, admins, super admins, the admin token and local mode. Break-glass runs too.",
  BREAK_GLASS_TAG: "break_glass",
  BREAK_GLASS_BODY: "A break-glass run lifts team walls and restrictions only, never the Wardyn boundary. It needs a reason, is capped at the run-limit maximum and is tagged in the run and the audit log.",

  // The pinned footer.
  FOOTER: "Every save shows a dry run first. Changing the mode needs a second admin.",
  DISCARD: "Discard",
  REVIEW: "Review changes",

  /** not on the boards: the page's load failure. */
  FETCH_FAILED_TITLE: "Couldn't load the Wardyn boundary",
  /** not on the boards: a direct save (no second admin required on this deployment). */
  SAVED: "Saved.",
} as const;

// The dry run (board T4's report anatomy, for the Wardyn boundary).
export const BOUNDARY_DRY_RUN = {
  /** not on the boards: T4 titles name a team change; this one names the boundary. */
  TITLE: "What the Wardyn boundary as drafted would change",
  /** not on the boards: T4's "Nothing has changed yet." lead, worded for the boundary. */
  LEAD: "Nothing has changed yet. This compares the Wardyn boundary today with your draft, for the Default team, every team, stored policy and person.",
  CHANGE: "The change",
  TODAY: "Today",
  AFTER: "After approval",
  MODE: "Mode",
  /** not on the boards: tile labels for the boundary report's counts. */
  TILE_TEAMS: "teams narrow",
  TILE_POLICIES: "stored policies narrow",
  TILE_PEOPLE: "people narrow",
  TEAMS: "Teams below",
  /** not on the boards. */
  POLICIES: "Stored policies",
  PEOPLE: "People",
  DEFAULT_TEAM: "Default",
  NARROWED: "Narrowed",
  NO_CHANGE: "No change",
  COMPARED: "Compared with Wardyn's walls only. Your organisation's boundary isn't included.",
  BACK: "Back to editing",
  SUBMIT: "Submit for approval",
  /** not on the boards: the report's own failure (M1's approved wording, before the tree rename). */
  FAILED_TITLE: "Couldn't build the report",
  /** not on the boards. */
  FAILED_BODY: "Nothing was changed. You can't submit until the report loads.",
  RETRY: "Try again",
  // A knob the report names, by its wire name. One the console predates shows its raw name.
  KNOB: {
    allowed_domains: "Allowed hosts",
    denied_domains: "Blocked hosts and ranges",
    min_confinement_class: "Confinement floor",
    eligible_grants: "Credentials",
    "limits.max_end_ahead_sec": "Longest run",
  } as Record<string, string>,
} as const;
