/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LOGIN SANDBOX NOTE — what this box IS, on the one run page that never said.
//
// `harness login` is a server-side task discriminator (harnessLoginTask,
// internal/api/harnesscred.go): it gates the credential upload route, keeps the
// session out of the recorder, and pins the run to an image whose own Dockerfile
// header says "NOT a coding agent". The console labelled it nowhere. Opening it
// from /runs therefore looked like any other interactive run — a bare shell, no
// agent, no task — and the operator's reasonable next move (type `aws sso login`
// on its own) leaves the token in ~/.aws/sso/cache, where it dies with the
// container: `wardyn-aws-sso` is what uploads it, and the console's own pane is
// what normally types the two as one chained command.
//
// Its own file because run-detail.tsx is six lines under the 1000-line cap
// (scripts/check-file-size.sh) and this is a whole surface, not a line.
import { KeyRound } from "lucide-react";
import type { AgentRun } from "../../../lib/types";

// The server-side task literal this note keys on. Mirrors harnessLoginTask
// (internal/api/harnesscred.go) — a discriminator, never client input.
export const HARNESS_LOGIN_TASK = "harness login";

// DRAFT (M2 canon pending) — says the three things the page could not: what the
// box is, where the sign-in that actually captures happens, and that nobody has
// to clean it up. Deliberately does NOT tell the operator to type the chained
// command here: the sandbox's own attach shell prints it (deploy/images/aws-sso),
// and the console pane types it, so this page has no fourth copy of it.
export const LOGIN_SANDBOX_NOTE =
  "AWS sign-in sandbox — the AWS CLI and nothing else. Sign in from Getting Started so the capture uploads; the sandbox closes itself when it is done.";

// Renders nothing for every other run, so nothing on this page moves unless the
// run really is a login box.
export function LoginSandboxNote({ run }: { run: AgentRun }) {
  if (run.task !== HARNESS_LOGIN_TASK) return null;
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
