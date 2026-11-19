/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the run page says about a run whose data_out rule keeps its content in. The server's own sentences
// (the recording refusal, the unreadable rule, the Files panel's unavailable answer) are printed as sent and
// never defined here.
export const DATA_OUT_RUN = {
  TERMINAL: "Copying out of this terminal is off: this run keeps its content in. You can still read it.",
  // The page's own read of the rule failed, so whether the run keeps its content in is not known.
  TERMINAL_UNREAD: "Copying out of this terminal is off: Wardyn could not read this run's `data_out` rule.",
  FILES_BLOCKED: "This run's data_out rule keeps its content in.",
  FILES_NO_DESKTOP: "This run has no desktop. Files it saves in the transfer folder can leave one at a time, each with a person's yes.",
  FOLDER_IN_RUN: "in the run",
  RECORDING_TITLE: "This run's recording stays in",
  RETRY: "Try again",
} as const;
