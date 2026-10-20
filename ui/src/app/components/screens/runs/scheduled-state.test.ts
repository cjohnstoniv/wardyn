/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { SCHEDULED_RUN } from "../../wardyn/copy/scheduled-runs";
import { scheduledBadgeWord, scheduledMeta, scheduledWait, signInTimedOut } from "./scheduled-state";
import type { AgentRun, RunState } from "../../../lib/types";

const queue = (reason: string, over: object = {}) => ({
  pool_id: "p", queued_at: "2026-10-20T14:00:00Z", deadline_at: "2026-10-27T14:00:00Z", position: 0, reason,
  start_at: "2026-10-21T02:00:00Z", time_zone: "UTC", ...over,
});
const queued = (reason: string, over: object = {}) => ({ state: "QUEUED" as RunState, queue: queue(reason, over) });

describe("scheduledWait", () => {
  it("knows the two scheduled waits by the server's reasons, on a QUEUED run only", () => {
    expect(scheduledWait(queued("scheduled"))).toBe("scheduled");
    expect(scheduledWait(queued("owner_sign_in"))).toBe("sign_in");
    expect(scheduledWait(queued("runner_pool_at_capacity"))).toBeNull();
    expect(scheduledWait({ state: "RUNNING" as RunState, queue: queue("scheduled") })).toBeNull();
    expect(scheduledWait({ state: "QUEUED" as RunState })).toBeNull();
  });
});

describe("scheduledMeta", () => {
  it("says starts {when} for a scheduled run and was due {when} for one waiting for its owner, in the zone it was given in", () => {
    expect(scheduledMeta(queued("scheduled"))).toBe("starts Wed 21 Oct, 02:00 (UTC)");
    expect(scheduledMeta(queued("owner_sign_in"))).toBe("was due Wed 21 Oct, 02:00 (UTC)");
    expect(scheduledMeta(queued("scheduled", { time_zone: "Europe/London" }))).toBe("starts Wed 21 Oct, 03:00 (Europe/London)");
    expect(scheduledMeta(queued("scheduled"))).toBe(SCHEDULED_RUN.ROW_STARTS("Wed 21 Oct, 02:00 (UTC)"));
  });

  it("says nothing without a start time, or for another wait", () => {
    expect(scheduledMeta(queued("scheduled", { start_at: undefined }))).toBeUndefined();
    expect(scheduledMeta(queued("runner_pool_at_capacity"))).toBeUndefined();
  });
});

describe("scheduledBadgeWord", () => {
  it("is Scheduled or Waiting for sign-in on those waits and the state's own word otherwise", () => {
    expect(scheduledBadgeWord(queued("scheduled"))).toBe("Scheduled");
    expect(scheduledBadgeWord(queued("owner_sign_in"))).toBe("Waiting for sign-in");
    expect(scheduledBadgeWord(queued("runner_pool_at_capacity"))).toBeUndefined();
  });
});

describe("signInTimedOut", () => {
  it("is a FAILED run whose status reason is the sign-in timeout, not an ordinary queue timeout", () => {
    expect(signInTimedOut({ state: "FAILED" as RunState, status_reason: "queue_sign_in_timeout" } as Pick<AgentRun, "state" | "status_reason">)).toBe(true);
    expect(signInTimedOut({ state: "FAILED" as RunState, status_reason: "queue_timeout" })).toBe(false);
    expect(signInTimedOut({ state: "KILLED" as RunState, status_reason: "queue_sign_in_timeout" })).toBe(false);
  });
});
