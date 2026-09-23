/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one nav label that app-shell.tsx (eager) needs from governance-copy.ts
// (screen-only canon, reached only through lazy governance/drives/providers/
// setup screens). Rollup can't tree-shake an OBJECT's individual properties —
// pulling GOVERNANCE.TITLE off the big canon table pulled the whole §7.2-§7.9
// object into the entry chunk (#498). governance-copy.ts's GOVERNANCE.TITLE
// reads this constant, so it stays the one string for both the nav label and
// the screen heading, per governance-prompt.md §7.2.
export const GOVERNANCE_NAV_TITLE = "Governance";

// Same reasoning, one entry later (UT-7a): the User types screen's own
// heading (user-types-copy.ts's USER_TYPES.TITLE) reads this constant too, so
// the nav label and the screen heading can't drift apart.
export const USER_TYPES_NAV_TITLE = "User types";
