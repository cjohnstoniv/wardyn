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
} from "./login-start-wait";

// Finding 6 (0.7.4 field report): the pane's readiness budget was 15 consecutive
// failed polls, called "≈30s". These are the four answers that replace it, as a
// table — the component test drives the same rules through a real poll loop, but
// the RULES are decided here.
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

  // The measured cold pull was 131s. A healthy read every two seconds for two
  // minutes is the case the old budget could not express at all.
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

  // R1-F8: the same blip PAST the slow-start window is still "starting", never
  // "slow" — the slow-start sentence claims Wardyn CAN read the sandbox, and the
  // read it would be claiming that about has just failed.
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
