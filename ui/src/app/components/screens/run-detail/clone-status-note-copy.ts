/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's repository clone note: the approved board's strings, character for character
// (clone-status-note.test.tsx pins them). The not-reported row names Wardyn's gap, never the sandbox:
// Wardyn may simply not have read the file.
export const CLONE_NOTE = {
  titleSomeNotCloned: "Not every repository was cloned",
  titleNotReported: "Some repositories were not reported",
  chipSkipped: "Skipped",
  chipFailed: "Failed",
  // Its own key; the same text as runner-pools.ts's chip, never that key.
  notReported: "Not reported",
  footer: "Skipped and failed rows are the sandbox's own report.",
  notReportedRow: "Wardyn got no outcome for this repository, so it may or may not have been cloned.",
  // The first words of the two sentences below, for a cause this console does not know.
  skippedBare: "Skipped.",
  failedBare: "Not cloned.",
} as const;

// The launcher's cause token, in the person's words.
export const CLONE_CAUSE_SENTENCE: Record<string, string> = {
  not_a_checkout:
    "Skipped. The folder already holds files that are not a checkout of this repository, and they were left as they are.",
  cannot_create: "Skipped. The folder could not be created, for example on a read-only mount.",
  clone_failed: "Not cloned. The run's allowed hosts or Wardyn's credential broker may have blocked the clone.",
  ref_failed:
    "Not cloned. The ref may not exist, or the run's allowed hosts or Wardyn's credential broker may have blocked the clone.",
};
