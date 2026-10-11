/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Who governs this install (docs/design/0.9/PLAN.md §10.3). "Offline" and
// "ungoverned" are never the same word. The account menu names the tier
// always; the top bar carries a chip only for local-only (RN-Q26).
export const TIER = {
  LABEL: {
    "local-only": "Local only — not governed by an organisation",
    runner: "Runner for an organisation",
    org: "Organisation",
  },
  CHIP: "Local only",
  MENU_PREFIX: "Tier",
} as const;
