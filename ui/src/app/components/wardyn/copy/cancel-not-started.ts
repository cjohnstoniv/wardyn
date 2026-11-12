/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Cancel run on a run that has not started: scheduled for later, or waiting for its owner's sign-in. Nothing is set up
// for it (SCHEDULED_RUN.PAGE_NOTE, SIGN_IN_BODY), so the confirm does not talk about a sandbox or credentials. A run
// that waits for a pool slot, and a run that is running, keep the End run confirm.
export const CANCEL_NOT_STARTED = {
  TITLE: (name: string) => `Cancel ${name}?`,
  BODY: "It will not start. Nothing was set up.",
  CONFIRM: "Cancel run",
  KEEP: "Keep run",
  DONE: (name: string) => `Cancelled ${name}`,
  FAILED: (name: string) => `Couldn't cancel ${name}`,
} as const;
