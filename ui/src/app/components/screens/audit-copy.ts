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
  // What a credential.* row says about a person's stored sign-in. A
  // credential.revoke row records the end of a run's OWN credentials and
  // touches no stored sign-in, with or without its `scope` field (rows written
  // before it existed are the ones that misled).
  REVOKE_RUN_CREDENTIALS: "Recorded the end of this run's own credentials. The person's sign-in is not affected.",
  EXPIRED_DELETE_REFUSED: "Removed a stored sign-in the provider refused",
  EXPIRED_DELETE_LOST_REPLY:
    "Removed a stored sign-in: the provider may have accepted a renewal whose reply was lost",
  EXPIRED_DELETE_EXPIRED: "Removed a stored sign-in that had expired",
  // The same three rows with a failure outcome: the deletion did not happen
  // and the sign-in is still stored.
  EXPIRED_DELETE_REFUSED_FAILED: "Could not remove a stored sign-in the provider refused",
  EXPIRED_DELETE_LOST_REPLY_FAILED:
    "Could not remove a stored sign-in after a renewal whose reply may have been lost",
  EXPIRED_DELETE_EXPIRED_FAILED: "Could not remove a stored sign-in that had expired",
} as const;
