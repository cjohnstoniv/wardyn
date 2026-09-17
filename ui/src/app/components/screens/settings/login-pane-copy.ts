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

// DRAFT (M2 canon pending) — P5: POST /setup/harness-login answers with the run
// id BEFORE the sandbox exists (internal/api/harnesscred_launch.go), so the pane
// has a real wait to narrate; it used to mount the terminal on a run that was
// still PENDING, which handleAttachTicket 409s, and a failed mint is TERMINAL in
// AttachTerminal — the operator's only signal was a dead panel.
//
// U-12 (W6 blind lens): it named the wait "the first start after an upgrade",
// which is false on a FIRST install (nothing was upgraded), and named a pull
// that does not happen on a Docker/compose host with the image already local. It
// is also shown for the anthropic flow, whose image is the ordinary agent one.
// What is true of every one of those is that a first start MAY pull.
//
// Declared HERE, not in the pane: ui/e2e/providers.spec.ts asserts through it
// (U-15) and a Playwright spec cannot import the pane — it reaches
// AttachTerminal's xterm.css, which Node's loader cannot load. The pane
// re-exports it.
export const LOGIN_SANDBOX_STARTING =
  "Starting the sign-in sandbox. A first start may need to pull the image, which can take a few minutes.";

// DRAFT (M2 canon pending) — U-8: the aws blurb's opening clause under
// `startURLManaged` (every per_user member). The unmanaged clause asks the
// reader to give Wardyn their organization's access portal URL — and under a
// managed row there is no field to give it in, the server ignores a supplied one
// (harnessLogin uses the row's own sso_start_url), and the intro one line above
// has just said there is nothing to enter. The rest of the blurb is unchanged:
// the sandbox, the ~/.aws/config it writes and the command it runs are the same.
export const AWS_BLURB_MANAGED_OPENING =
  "Your admin set your organization's AWS access portal; there is nothing to enter.";

// DRAFT (M2 canon pending) — the first line the aws-sso image's sign-in pane
// prints (deploy/images/aws-sso/login-hint.sh's banner starts with it). The
// console types the login command only if this never appears; see the pane's
// grace-timer comment for why.
export const SELFRUN_MARKER = "wardyn: sign-in running";
