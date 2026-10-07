/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which run is an AWS sign-in run, and who may read its waiting sign-in.
//
// Its own file because the Runs list asks this for every row: in
// login-sandbox-note.tsx it pulled the run page's note into the entry chunk
// (bundle-split.test.ts).
import type { AgentRun } from "../../../lib/types";
import { mayEnterRun } from "../../../lib/run-entry";

// The server-side task literal the login-sandbox note keys on. Mirrors harnessLoginTask
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

// Whether this viewer's console may read this run's sign-in: a RUNNING AWS
// sign-in run they may enter. `mayEnterRun`, never the unknown-identity arm,
// which would fire a read the server then refuses and draw a false failure.
export function mayReadSignIn(run: AgentRun, principal: string, operator: boolean): boolean {
  return (
    run.task === HARNESS_LOGIN_TASK &&
    run.agent === AWS_SSO_LOGIN_AGENT &&
    run.state === "RUNNING" &&
    mayEnterRun(run, principal, operator)
  );
}
