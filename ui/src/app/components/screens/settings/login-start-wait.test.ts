/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  startWaitVerdict,
  RUN_POLL_MIN_FAILURES,
  RUN_POLL_RETRYING_AFTER_MS,
  RUN_POLL_SLOW_START_MS,
  RUN_POLL_UNREADABLE_AFTER_MS,
  LOGIN_SANDBOX_SLOW_START,
  LOGIN_SANDBOX_READ_RETRYING,
  LOGIN_SANDBOX_STUCK_LEAD_IN,
  type StartWaitInput,
} from "./login-start-wait";
import { TERMINAL_STATUS_REASONS } from "../run-status-detail";

// Finding 6 (0.7.4 field report): the pane's readiness wait is graded into four
// answers by this table, not a single fixed poll-count budget — the component
// test drives the same rules through a real poll loop, but the RULES are
// decided here.
const T0 = 1_000_000;

describe("startWaitVerdict", () => {
  it("is 'starting' for a healthy wait inside the slow-start window", () => {
    expect(startWaitVerdict({ now: T0, startedAt: T0, failingSince: null, failures: 0 })).toBe("starting");
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_SLOW_START_MS - 1,
        startedAt: T0,
        failingSince: null,
        failures: 0,
      }),
    ).toBe("starting");
  });

  // The measured cold pull was 131s: a healthy read every two seconds for two
  // minutes must still grade as merely slow, not a failure.
  it("is 'slow' once a HEALTHY wait passes the slow-start window", () => {
    expect(
      startWaitVerdict({ now: T0 + RUN_POLL_SLOW_START_MS, startedAt: T0, failingSince: null, failures: 0 }),
    ).toBe("slow");
    expect(startWaitVerdict({ now: T0 + 131_000, startedAt: T0, failingSince: null, failures: 0 })).toBe("slow");
  });

  // A blip is not an outage: below the retrying window the starting copy stands,
  // whatever the failure counter says.
  it("keeps the healthy copy for a short run of failed reads", () => {
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_RETRYING_AFTER_MS - 1,
        startedAt: T0,
        failingSince: T0,
        failures: 5,
      }),
    ).toBe("starting");
  });

  // The same blip past the slow-start window is still "starting", never "slow"
  // — the slow-start sentence claims Wardyn CAN read the sandbox, and the read
  // it would be claiming that about has just failed.
  it("never claims the sandbox is readable while the latest read failed", () => {
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_SLOW_START_MS + 5_000,
        startedAt: T0,
        failingSince: T0 + RUN_POLL_SLOW_START_MS,
        failures: 2,
      }),
    ).toBe("starting");
  });

  it("is 'retrying' once reads have been failing for the retrying window", () => {
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_RETRYING_AFTER_MS,
        startedAt: T0,
        failingSince: T0,
        failures: 5,
      }),
    ).toBe("retrying");
    // …and a slow wait that STOPS being readable says so rather than staying on
    // the slow-start sentence: a 300s budget must not narrate a dead daemon as
    // "still starting".
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_SLOW_START_MS + RUN_POLL_RETRYING_AFTER_MS,
        startedAt: T0,
        failingSince: T0 + RUN_POLL_SLOW_START_MS,
        failures: 5,
      }),
    ).toBe("retrying");
  });

  it("is 'unreadable' only when BOTH the clock and the failure floor agree", () => {
    const failingFor = RUN_POLL_UNREADABLE_AFTER_MS;
    expect(
      startWaitVerdict({
        now: T0 + failingFor,
        startedAt: T0,
        failingSince: T0,
        failures: RUN_POLL_MIN_FAILURES,
      }),
    ).toBe("unreadable");
    // usePoll skips ticks while the tab is hidden, so a tab that comes back
    // after ten minutes and fails ONE read is past the clock with a single
    // failure. The floor is what stops that ending a working sign-in.
    expect(
      startWaitVerdict({ now: T0 + failingFor, startedAt: T0, failingSince: T0, failures: 1 }),
    ).toBe("retrying");
    // One tick short of the clock, with failures to spare.
    expect(
      startWaitVerdict({
        now: T0 + failingFor - 1,
        startedAt: T0,
        failingSince: T0,
        failures: RUN_POLL_MIN_FAILURES * 10,
      }),
    ).toBe("retrying");
  });

  // A successful read clears failingSince, which is the ONLY way the unreadable
  // clock is reset — the component holds that invariant, so the table states it.
  it("never ends the wait while reads are succeeding, however long it takes", () => {
    expect(
      startWaitVerdict({
        now: T0 + RUN_POLL_UNREADABLE_AFTER_MS * 10,
        startedAt: T0,
        failingSince: null,
        failures: 0,
      }),
    ).toBe("slow");
  });

  // The two new sentences are the lane's DRAFT copy; pinned so a reword is a
  // deliberate act with a canon row behind it.
  it("carries the two sentences the new gradings need", () => {
    expect(LOGIN_SANDBOX_SLOW_START).toContain("Wardyn can read the sign-in sandbox, it just isn't up yet");
    expect(LOGIN_SANDBOX_READ_RETRYING).toContain("still trying");
    expect(RUN_POLL_UNREADABLE_AFTER_MS).toBe(300_000);
  });
});

// 0.7.6 finding 6: the wait stops being graded purely on a clock. The substrate
// already knows whether this is normal or over, and the pane now reads it.
describe("startWaitVerdict — the REASON, not the clock", () => {
  it("ends the wait at two seconds on a terminal reason", () => {
    // "ImagePullBackOff for two seconds is terminal" — a terminal reason ends
    // the wait immediately, not after the slow-start clock runs out.
    expect(
      startWaitVerdict({
        now: T0 + 2_000,
        startedAt: T0,
        failingSince: null,
        failures: 0,
        detail: "agent: ImagePullBackOff: rpc error: pull access denied",
        reason: "ImagePullBackOff",
      }),
    ).toBe("stuck");
  });

  it("every reason the substrate calls terminal ends it, not just the image ones", () => {
    // Every reason the substrate calls terminal ends the wait, not just the
    // image ones — which is why the lead-in does not say "image".
    for (const reason of TERMINAL_STATUS_REASONS) {
      expect(
        startWaitVerdict({
          now: T0 + 1_000,
          startedAt: T0,
          failingSince: null,
          failures: 0,
          detail: `agent: ${reason}: something`,
          reason,
        }),
      ).toBe("stuck");
    }
    expect(LOGIN_SANDBOX_STUCK_LEAD_IN).not.toContain("image");
  });

  it("does NOT end the wait on an ordinary reason, however long it has run", () => {
    // "ContainerCreating for two minutes is normal" — the other half of the
    // same sentence. The clock is untouched: this is still 'slow'.
    expect(
      startWaitVerdict({
        now: T0 + 120_000,
        startedAt: T0,
        failingSince: null,
        failures: 0,
        detail: "agent: ContainerCreating",
        reason: "ContainerCreating",
      }),
    ).toBe("slow");
    expect(
      startWaitVerdict({
        now: T0 + 5_000,
        startedAt: T0,
        failingSince: null,
        failures: 0,
        detail: "pod: Unschedulable: 0/1 nodes are available",
        reason: "Unschedulable",
      }),
    ).toBe("starting");
  });

  // A run with no status_detail — a warm Docker image, a pre-0.7.6 daemon —
  // must grade exactly as it did in 0.7.5. The rows are the four cases above,
  // re-run with the new inputs absent and then explicitly null.
  it("grades a run with no detail exactly as 0.7.5 did", () => {
    const rows: Array<[StartWaitInput, string]> = [
      [{ now: T0, startedAt: T0, failingSince: null, failures: 0 }, "starting"],
      [{ now: T0 + RUN_POLL_SLOW_START_MS, startedAt: T0, failingSince: null, failures: 0 }, "slow"],
      [{ now: T0 + RUN_POLL_RETRYING_AFTER_MS, startedAt: T0, failingSince: T0, failures: 3 }, "retrying"],
      [
        {
          now: T0 + RUN_POLL_UNREADABLE_AFTER_MS,
          startedAt: T0,
          failingSince: T0,
          failures: RUN_POLL_MIN_FAILURES,
        },
        "unreadable",
      ],
    ];
    for (const [input, want] of rows) {
      expect(startWaitVerdict(input)).toBe(want);
      expect(startWaitVerdict({ ...input, detail: null, reason: null })).toBe(want);
      expect(startWaitVerdict({ ...input, detail: "" })).toBe(want);
    }
  });
});
