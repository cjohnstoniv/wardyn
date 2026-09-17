/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in pane's strings that something OUTSIDE the browser bundle has to
// read: the live kind walk (ui/e2e/live/*) asserts through them, and the Go
// parity test (cmd/wardyn-aws-sso) pins SELFRUN_MARKER against the image's
// banner. They live in a module with NO imports on purpose. harness-login-pane
// reaches AttachTerminal, which imports xterm's stylesheet, and Playwright's
// Node loader cannot load a .css file — importing a constant from the pane made
// the whole live walk fail with "No tests found" while typecheck stayed green.
// harness-login-pane.tsx re-exports both, so the console and vitest are unmoved.

// DRAFT (M2 canon pending) — the wait ending because Wardyn can no longer READ
// the run (a daemon restart mid-pull, a pruned run, a 403 after a roster edit).
// Distinct from the "stopped before it was ready" sentence on purpose: that one
// asserts the sandbox stopped, which the pane has not established — all it
// knows is that it stopped being able to ask.
export const LOGIN_SANDBOX_UNREADABLE =
  "Wardyn stopped being able to read the sign-in sandbox, so it can't say whether it came up. Try again.";

// DRAFT (M2 canon pending) — the first line the aws-sso image's sign-in pane
// prints (deploy/images/aws-sso/login-hint.sh's banner starts with it). The
// console types the login command only if this never appears; see the pane's
// grace-timer comment for why.
export const SELFRUN_MARKER = "wardyn: sign-in running";
