/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DR-0020: the cross-tab event is a nonsecret invalidation. A receiver trusts none of its content for
// who is signed in, drops repeats, older events and events a later /me read already saw, and turns a
// burst into one re-check.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeBroadcastChannel } from "../../../test/fake-broadcast-channel";
import { announceAuthChange, AUTH_CHANNEL, COALESCE_MS, noteIdentityRead, onRemoteAuthChange, resetAuthSyncForTest } from "./auth-sync";

const event = (over: Record<string, unknown> = {}) => ({ v: 1, tab: "other", seq: 1, at: Date.now(), kind: "changed", ...over });
const deliver = (data: unknown) => FakeBroadcastChannel.deliver(AUTH_CHANNEL, data);

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("BroadcastChannel", FakeBroadcastChannel);
  FakeBroadcastChannel.reset();
  resetAuthSyncForTest();
});
afterEach(() => {
  resetAuthSyncForTest();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function listen() {
  const seen: string[] = [];
  const stop = onRemoteAuthChange((kind) => seen.push(kind));
  return { seen, stop };
}
const settle = () => vi.advanceTimersByTime(COALESCE_MS + 1);

describe("announceAuthChange", () => {
  it("posts only the kind, the tab, a count and a time: no principal, role or token", () => {
    announceAuthChange("signed_out");
    announceAuthChange("changed");
    const [first, second] = FakeBroadcastChannel.posted;
    expect(first.name).toBe(AUTH_CHANNEL);
    expect(Object.keys(first.data as object).sort()).toEqual(["at", "kind", "seq", "tab", "v"]);
    expect(first.data).toMatchObject({ v: 1, seq: 1, kind: "signed_out" });
    expect(second.data).toMatchObject({ seq: 2, kind: "changed" });
    expect((first.data as { tab: string }).tab).toBe((second.data as { tab: string }).tab);
  });

  it("does nothing, without throwing, where there is no BroadcastChannel", () => {
    vi.stubGlobal("BroadcastChannel", undefined);
    resetAuthSyncForTest();
    expect(() => announceAuthChange("changed")).not.toThrow();
  });
});

describe("receiving", () => {
  it("tells a subscriber once, after the burst", () => {
    const { seen } = listen();
    deliver(event({ seq: 1 }));
    deliver(event({ seq: 2 }));
    deliver(event({ seq: 3 }));
    expect(seen).toEqual([]);
    settle();
    expect(seen).toEqual(["changed"]);
  });

  it("a deliberate sign-out in the burst wins over the rest, in either order", () => {
    for (const kinds of [["changed", "signed_out"], ["signed_out", "changed"]]) {
      const { seen, stop } = listen();
      kinds.forEach((kind, i) => deliver(event({ tab: `t-${kinds.join("")}`, seq: i + 1, kind })));
      settle();
      expect(seen).toEqual(["signed_out"]);
      stop();
    }
  });

  it("drops a repeated or older event from the same tab, and keeps another tab's", () => {
    const { seen } = listen();
    deliver(event({ seq: 5 }));
    settle();
    deliver(event({ seq: 5 }));
    deliver(event({ seq: 4, kind: "signed_out" }));
    settle();
    expect(seen).toEqual(["changed"]);
    deliver(event({ tab: "third", seq: 1 }));
    settle();
    expect(seen).toEqual(["changed", "changed"]);
  });

  it("drops an event that finished before this tab's latest /me read began: that read saw it", () => {
    const { seen } = listen();
    noteIdentityRead(10_000);
    deliver(event({ seq: 1, at: 9_999 }));
    settle();
    expect(seen).toEqual([]);
    deliver(event({ seq: 2, at: 10_000 }));
    settle();
    expect(seen).toEqual(["changed"]);
  });

  it("ignores anything that is not exactly an auth event, and anything an event carries beyond it", () => {
    const { seen } = listen();
    const bad = [null, "changed", 7, [], {}, event({ v: 2 }), event({ tab: "" }), event({ seq: 0 }), event({ seq: 1.5 }), event({ at: Number.NaN }), event({ kind: "signed_in" }), event({ kind: undefined })];
    bad.forEach(deliver);
    settle();
    expect(seen).toEqual([]);
    deliver(event({ tab: "extra", principal: "mallory", role: "admin" }));
    settle();
    expect(seen).toEqual(["changed"]);
  });

  it("does not hear its own tab", () => {
    announceAuthChange("changed");
    const tab = (FakeBroadcastChannel.posted[0].data as { tab: string }).tab;
    const { seen } = listen();
    deliver(event({ tab, seq: 99 }));
    settle();
    expect(seen).toEqual([]);
  });

  it("stops telling a subscriber that has unsubscribed", () => {
    const { seen, stop } = listen();
    stop();
    deliver(event());
    settle();
    expect(seen).toEqual([]);
  });
});
