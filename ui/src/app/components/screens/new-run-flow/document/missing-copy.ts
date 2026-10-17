/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TODO(E1d follow-up): the canon (copy/new-run/document.ts) still has no words for a roster with no enabled
// harness, so that case renders nothing. Normal use never reaches it. Replace the value with its NR_DOCUMENT
// constant when it lands. (The too-large and unreadable cases have theirs: FR-0098.)
export const MISSING_COPY = {
  NO_HARNESS: "",
} as const;
