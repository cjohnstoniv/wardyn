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

// Canon (#628, the approved sign-in progress packet) — the door's own progress,
// ready and opened states. The click that opens the dialog opens no tab any
// more: the provider tab opens only from the "Open … sign-in" button, once its
// page exists. `provider` is the flow's short name ("AWS", "Claude"). Here, not
// in the pane, because the live walk asserts through them.
export const SIGNIN_PROGRESS = {
  STEP_START: "Starting the sign-in sandbox",
  STEP_DOWNLOAD: "Downloading the sign-in image",
  STEP_DOWNLOAD_ACTIVE: "Downloading the sign-in image — first time only",
  STEP_DOWNLOAD_FAILED: "Downloading the sign-in image — failed",
  STEP_WAIT: (provider: string) => `Waiting for ${provider}`,
  // No runner reports pull progress today (the docker driver drains the pull
  // stream, the kubelet reports none), so this sentence is the whole of state 2.
  DOWNLOAD_HINT: "Can take a few minutes the first time.",
  WAIT_HINT: (provider: string) => `Waiting on ${provider} to hand back a verification link.`,
  OPEN: (provider: string) => `Open ${provider} sign-in`,
  COPY_LEAD: "If nothing opens:",
  COPY_LINK: "copy the link",
  TAB_OPEN: (provider: string) => `The ${provider} sign-in tab is open. Waiting for your approval there.`,
  REOPEN: "Reopen tab",
  RETRY: "Retry",
  CANCEL: "Cancel",
} as const;
