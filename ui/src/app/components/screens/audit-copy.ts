/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #459 — docs/design/announced-failures-canon.md's AUDIT.MEMBER_FEED_TITLE.
// Pulled out of audit.tsx's inline JSX literal (W25-W25.2-3's member-feed
// EmptyState title) into its own constant, byte-identical, so it can be
// pinned against the canon doc (owner ruling 2026-09-25, #726).
export const AUDIT = {
  MEMBER_FEED_TITLE: "The full audit feed is admin-only.",
  // Mock packet M4 (approved 2026-10-03), surface D: a sealed field whose key
  // was destroyed.
  ERASED: "Erased",
  ERASED_HINT: "Erased on request. The event and its place in the log remain.",
} as const;
