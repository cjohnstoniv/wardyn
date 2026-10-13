/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TODO(E1d follow-up): the canon (copy/new-run/document.ts) has no words for these three cases yet, so each
// renders nothing. Normal use never reaches them: a file or document over 1 MiB, a file the browser cannot
// read, and a roster with no enabled harness. Replace each value with its NR_DOCUMENT constant when it lands.
export const MISSING_COPY = {
  TOO_LARGE: "",
  FILE_UNREADABLE: "",
  NO_HARNESS: "",
} as const;
