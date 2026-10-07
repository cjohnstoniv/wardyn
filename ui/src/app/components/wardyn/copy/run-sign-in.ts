/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's waiting-sign-in strip (approved 088 mock, M2). Imports nothing:
// the Runs list reaches its row word through copy/runs-landing.ts instead, since
// runs-model.ts is on the eager graph.
export const RUN_SIGN_IN = {
  TITLE: "This sign-in is waiting for you",
  BODY: "Approve it on the verification page. If the page asks for a code, enter this one.",
  CODE_LABEL: "Verification code",
  OPEN: "Open the verification page",
  COPY: "Copy code",
  // The host renders mono at the call site: split on it, never rebuilt.
  OPENS: (host: string) => `Opens ${host}`,
  CHECKING: "Checking for a waiting sign-in…",
  NO_LONGER: "This sign-in is no longer waiting. The terminal below shows how it ended.",
  READ_FAILED: "Couldn't check whether a sign-in is waiting. The terminal below still shows it.",
} as const;
