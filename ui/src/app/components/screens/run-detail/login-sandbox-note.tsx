/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What this box is, on the one run page that never said so.
//
// `harness login` is a server-side task discriminator (harnessLoginTask,
// internal/api/harnesscred.go): it gates the credential upload route, keeps the
// session out of the recorder, and pins the run to an image whose own Dockerfile
// header says "NOT a coding agent". Without this note, opening it from /runs
// looks like any other interactive run — a bare shell, no agent, no task —
// and the operator's reasonable next move (type `aws sso login` on its own)
// leaves the token in ~/.aws/sso/cache, where it dies with the
// container: `wardyn-aws-sso` is what uploads it, and the console's own pane is
// what normally types the two as one chained command.
//
// Its own file because run-detail.tsx is six lines under the 1000-line cap
// (scripts/check-file-size.sh) and this is a whole surface, not a line.
import { KeyRound } from "lucide-react";
import type { AgentRun } from "../../../lib/types";

// The server-side task literal this note keys on. Mirrors harnessLoginTask
// (internal/api/harnesscred.go) — a discriminator, never client input. Held to
// the Go constant by TestHarnessLoginTask_UIParity (internal/api), which reads
// this file: renaming one side alone silently stops the note rendering on the
// one run it exists for.
export const HARNESS_LOGIN_TASK = "harness login";

// …and the agent, because the task alone is provider-agnostic. Every container
// login sets `harness login` — the Anthropic lane is the route's own default
// (`provider = "anthropic"`, harnesscred_launch.go) and runs `claude setup-token`
// in the claude-code image. Keyed on the task alone, this note told a
// Claude-subscription login run it was an AWS box that runs the AWS CLI and
// nothing else, signed in from Getting Started — false on all three clauses.
// Mirrors awsSSOAgent (internal/api/harnesscred.go), pinned by the same test.
export const AWS_SSO_LOGIN_AGENT = "aws-sso";

// DRAFT (M2 canon pending) — says the three things the page could not: what the
// box is, what the terminal below is waiting for, and that nobody has to clean it
// up. The old sentence sent the reader to Getting Started to sign in, which was
// the right advice when this terminal was a bare shell and the console pane was
// the only thing that typed the chained command. The image runs it itself now
// (deploy/images/aws-sso/signin-pane.sh) and this terminal is attached to that very
// session, so telling the reader to start a SECOND sign-in elsewhere would be the
// one instruction guaranteed to waste their device code.
//
// U-2 (W6 blind lens) — and it claims nothing the page cannot know. "the sign-in
// is already running in this box" is a fact about the IMAGE, not about this run:
// an operator WARDYN_AGENT_IMAGES pin (what private estates use) makes a
// console-0.7.5 / image-0.7.4 pairing real, and on an image-0.7.4 sandbox reached
// from the Runs list nothing types the pair at all — the note then labelled
// finding 4's bare shell "already running". So the note points at the TERMINAL and
// names both shapes it can be in, which is true of either image. What the console
// does know is the box, and when it ends.
//
// "stops itself after 30 idle minutes" is that second fact. Nothing server-side
// stops a login run on capture — the shutdown is the console sign-in pane's own
// killRun, and a sandbox reached from the Runs list never gets one. What ends it is
// the reaper: the login policy takes its AutoStopAfterSec from harnessLoginIdleCap
// (internal/api/harnesscred.go), which internal/lifecycle reads as an IDLE timeout
// of 30 minutes — the one number an operator can plan around.
export const LOGIN_SANDBOX_NOTE =
  "AWS sign-in sandbox — the AWS CLI and nothing else. If the terminal shows a device code, finish it in your browser; if it shows a prompt, run the command the shell prints. It stops itself after 30 idle minutes.";

// Renders nothing for every other run, so nothing on this page moves unless the
// run really is a login box.
//
// U-2: …and only while that box is UP. Every clause above — the terminal, the
// device code, the idle cap — describes a RUNNING sandbox, and the Runs list is
// exactly where a KILLED or COMPLETED login run is reopened. A run that is still
// PENDING has no terminal to point at either.
export function LoginSandboxNote({ run }: { run: AgentRun }) {
  if (run.task !== HARNESS_LOGIN_TASK || run.agent !== AWS_SSO_LOGIN_AGENT) return null;
  if (run.state !== "RUNNING") return null;
  return (
    <div
      className="flex shrink-0 items-start gap-2 rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground"
      data-testid="login-sandbox-note"
    >
      <KeyRound className="mt-0.5 size-3.5 shrink-0 text-primary" />
      <p>{LOGIN_SANDBOX_NOTE}</p>
    </div>
  );
}
