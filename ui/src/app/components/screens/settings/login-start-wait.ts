/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in sandbox's wait is graded on the clock, not a tick count (0.7.4
// field report, finding 6). A tick count is not a duration: fast 5xx answers
// reach 15 in thirty seconds, while reads that hang reach it in fifteen
// MINUTES (each one costs WFETCH_TIMEOUT_MS), and `usePoll` skips ticks
// entirely while the tab is hidden. A 131-second first pull of the aws-sso
// image, with HEALTHY reads throughout — which never trips a failed-poll
// counter at all — showed that the budget itself was not the problem: one
// line was narrating three different waits.
//
// So the wait is graded on the clock, and the counter is kept as a FLOOR rather
// than as the measure: a hidden tab that comes back to one failed read must not
// end a sign-in that is five minutes old and working.
//
// What this file deliberately does NOT do is claim to know a pull is happening.
// Nothing on the read path can: `GET /runs/{id}` is a plain SELECT, the k8s
// canary reports `ContainerCreating` for a pull and for everything else, and
// "Pulling" is an Event reason the chart grants no verb to read. The slow-start
// sentence is hedged for that reason.
//
// 0.7.6 (finding 6) changes the SHAPE of that concession rather than the
// budget: the substrate's reason now reaches this file (`agent_runs.status_detail`),
// so the clock stops being the verdict and becomes the FALLBACK for when there
// is no reason to read. ImagePullBackOff for two seconds is terminal;
// ContainerCreating for two minutes is normal. A run with no detail — a Docker
// host whose image was already warm, a pre-0.7.6 daemon — grades exactly as it
// did in 0.7.5.
import { LAUNCH_DEADLINE_MS } from "../../../lib/api/core";
import { isTerminalStatusReason, parseStatusDetail } from "../run-status-detail";

// The wall-clock budget before the pane says it cannot read the run. The SAME
// number the console already spends on a call that brings a sandbox up
// (LAUNCH_DEADLINE_MS, lib/api/core.ts) — a sandbox this console is willing to
// wait five minutes to CREATE is one it should be willing to wait five minutes
// to READ, and a second hand-picked constant would be a second answer to one
// question.
export const RUN_POLL_UNREADABLE_AFTER_MS = LAUNCH_DEADLINE_MS;

// How long reads must be failing before the pane says so. Below this a failed
// read is a blip and the starting copy stands: a five-minute budget must not be
// silent about a daemon that went away ten seconds ago, and must not shout
// about one dropped request either.
export const RUN_POLL_RETRYING_AFTER_MS = 10_000;

// When a HEALTHY wait stops being ordinary. The measured cold pull was 131s, so
// a minute is comfortably inside "this is normal" and comfortably before the
// point where silence reads as a hang.
export const RUN_POLL_SLOW_START_MS = 60_000;

// The FLOOR under the clock, kept from the old tick budget. usePoll skips ticks
// while `document.hidden` (use-poll.ts), so a tab left in the background for ten
// minutes comes back, fails ONE read, and would otherwise be past every
// deadline above at once. Fifteen consecutive failures is evidence; one is not.
export const RUN_POLL_MIN_FAILURES = 15;

// DRAFT (M2 canon pending) — the healthy-but-slow wait. Hedged on purpose: the
// pane cannot PROVE a pull is what it is waiting on (see the header), so it
// states what it knows — reads are working, the run is not up — and names the
// pull as the usual cause rather than as the diagnosis.
//
// U-12 (W6 blind lens): "the first start after an upgrade pulls the image onto
// this node" asserted three things the pane does not know. On Docker/compose
// there is no node and, with the image already local, nothing pulls; on a first
// install nothing was upgraded; and the same sentence narrates the ANTHROPIC
// flow, whose image is the ordinary agent one. What holds in every one of those
// is that a first start MAY need to pull.
export const LOGIN_SANDBOX_SLOW_START =
  "Still starting — Wardyn can read the sign-in sandbox, it just isn't up yet. A first start may need to pull the image, which can take a few minutes.";

// DRAFT (M2 canon pending) — reads are failing, but not for long enough to give
// up. Without this a 300-second budget would show "Starting…" for five minutes
// while the daemon was down, which is the same lie the old 30s budget told in
// the other direction.
export const LOGIN_SANDBOX_READ_RETRYING =
  "Wardyn can't read the sign-in sandbox right now — still trying. It may be starting normally.";

// DRAFT (M2 canon pending) — the wait ending on a REASON rather than a clock.
// The sentence that follows is the SUBSTRATE's (statusDetailSentence); this is
// only the lead-in that names the speaker, as SANDBOX_REFUSAL_LEAD_IN does.
//
// It does NOT say "image". The same verdict fires for
// CreateContainerError, CreateContainerConfigError and CrashLoopBackOff, none of
// which a new image reference fixes.
export const LOGIN_SANDBOX_STUCK_LEAD_IN =
  "The sign-in sandbox cannot start — this needs your admin; trying again gets the same answer until they fix it.";

// The five states the wait can be in. `unreadable` and `stuck` are the two that
// END it — the first because Wardyn cannot see the run, the second because the
// substrate has already given its final answer.
export type StartWaitVerdict = "starting" | "slow" | "retrying" | "unreadable" | "stuck";

export type StartWaitInput = {
  now: number;
  // When this launch began (the POST's resolve), not when the component mounted.
  startedAt: number;
  // When the CURRENT run of consecutive failed reads began, or null if the last
  // read succeeded.
  failingSince: number | null;
  // How many consecutive reads have failed.
  failures: number;
  // The run's status_detail as of the last SUCCESSFUL read, or null — an absent
  // reason is the 0.7.5 case and grades on the clock alone. A failed read
  // carries no detail by construction, which is why a reason can never be stale
  // relative to what the caller believes about the run.
  detail?: string | null;
  // The server's derived status_reason, when it sent one (it is the authority;
  // the detail is parsed only for a pre-0.7.6 daemon).
  reason?: string | null;
};

// startWaitVerdict grades one poll tick. Pure, so the table is testable without
// a component, a clock or a poll loop.
export function startWaitVerdict({ now, startedAt, failingSince, failures, detail, reason }: StartWaitInput): StartWaitVerdict {
  // FIRST, and with no clock at all: a terminal reason is the substrate's final
  // answer, and waiting out a five-minute budget for one is the exact defect
  // finding 6 names ("ImagePullBackOff for two seconds is terminal"). It can
  // only be set from a read that SUCCEEDED, so it never races the failure arms
  // below.
  if (isTerminalStatusReason(parseStatusDetail(detail, reason).reason)) return "stuck";
  if (failingSince !== null) {
    const failingFor = now - failingSince;
    // BOTH bounds, never either: the clock says the outage is real, the counter
    // says it is not one hidden-tab tick pretending to be one.
    if (failingFor >= RUN_POLL_UNREADABLE_AFTER_MS && failures >= RUN_POLL_MIN_FAILURES) return "unreadable";
    if (failingFor >= RUN_POLL_RETRYING_AFTER_MS) return "retrying";
    // A blip — and NOT "slow" (R1-F8). The slow-start sentence says in so many
    // words that Wardyn CAN read the sign-in sandbox, which is false while the
    // latest read failed. Under the retrying window a brief outage falls back to
    // the neutral starting line rather than to a claim just contradicted.
    return "starting";
  }
  return now - startedAt >= RUN_POLL_SLOW_START_MS ? "slow" : "starting";
}
