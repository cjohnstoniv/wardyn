/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { usePoll } from "./use-poll";

describe("usePoll (auto-refresh hook)", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("invokes fn once per interval while not paused (and not immediately)", () => {
    const fn = vi.fn();
    renderHook(({ paused }) => usePoll(fn, 1000, paused), {
      initialProps: { paused: false },
    });
    // It's a BACKGROUND refresher — no immediate call; the caller does its own
    // initial load.
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it("does not fire while paused, and resumes when unpaused without resetting the timer", () => {
    const fn = vi.fn();
    const { rerender } = renderHook(({ paused }) => usePoll(fn, 1000, paused), {
      initialProps: { paused: true },
    });
    vi.advanceTimersByTime(3000);
    expect(fn).not.toHaveBeenCalled();

    rerender({ paused: false });
    vi.advanceTimersByTime(2000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("always calls the LATEST fn (a new closure each render doesn't drop ticks)", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook(({ fn }) => usePoll(fn, 1000, false), {
      initialProps: { fn: first },
    });
    vi.advanceTimersByTime(1000);
    expect(first).toHaveBeenCalledTimes(1);

    rerender({ fn: second });
    vi.advanceTimersByTime(1000);
    expect(second).toHaveBeenCalledTimes(1);
    expect(first).toHaveBeenCalledTimes(1); // not called again
  });

  // F128: `return () => clearInterval(id)` was the hook's ONLY leak guard and
  // nothing unmounted it — deleting that line left use-poll.test.ts and the
  // runs / approvals / run-detail suites (64 tests) green, while every screen
  // that navigates away kept polling the API forever. usePoll is the shared
  // auto-refresh primitive behind the Runs board, the run cockpit, approvals,
  // live-approvals, audit, workspace-detail and recording, so the leak is once
  // per visit, per screen, for the life of the tab.
  it("stops polling on unmount — the interval is torn down, not merely ignored", () => {
    const fn = vi.fn();
    const { unmount } = renderHook(() => usePoll(fn, 1000, false));
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);

    unmount();
    vi.advanceTimersByTime(10000);
    expect(fn).toHaveBeenCalledTimes(1);
    // …and the timer itself is gone, not just a callback nobody watches: a
    // surviving interval keeps firing for the life of the tab.
    expect(vi.getTimerCount()).toBe(0);
  });

  // The same cleanup, on the other path that runs it: intervalMs is the
  // effect's only dependency, so a caller that changes its cadence (a screen
  // that slows polling while backgrounded) rebuilds the timer — the old one
  // must go with it, or the two run concurrently and fn fires twice a tick.
  it("replaces the timer when intervalMs changes instead of leaving both running", () => {
    const fn = vi.fn();
    const { rerender } = renderHook(({ ms }) => usePoll(fn, ms, false), {
      initialProps: { ms: 1000 },
    });
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);

    rerender({ ms: 5000 });
    expect(vi.getTimerCount()).toBe(1);
    vi.advanceTimersByTime(4000); // the OLD 1s timer would have fired 4 more times
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("is a no-op when intervalMs <= 0", () => {
    const fn = vi.fn();
    renderHook(() => usePoll(fn, 0, false));
    vi.advanceTimersByTime(10000);
    expect(fn).not.toHaveBeenCalled();
  });
});

// R4-F074: usePoll fired on a bare setInterval with no notion of whether the
// previous invocation had settled, and no caller passes an AbortSignal — so a
// daemon slower than the interval stacked a full set of requests per tick
// (measured: 34 concurrent in-flight on /runs/:id at 12s backend latency, 97 at
// 40s). Browsers cap ~6 connections per origin, so the surplus queued in the
// browser and delayed the operator's own clicks.
describe("usePoll — the in-flight guard", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("drops a tick while the previous invocation is still outstanding", async () => {
    let settle: (() => void) | null = null;
    const fn = vi.fn(() => new Promise<void>((res) => (settle = res)));
    renderHook(() => usePoll(fn, 1000, false));

    await vi.advanceTimersByTimeAsync(1000);
    expect(fn).toHaveBeenCalledTimes(1);

    // Four more intervals go by with the first call still unsettled: not one
    // of them stacks a second request.
    await vi.advanceTimersByTimeAsync(4000);
    expect(fn).toHaveBeenCalledTimes(1);

    settle!();
    await vi.advanceTimersByTimeAsync(1000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("clears the guard when the poll REJECTS — a failing endpoint must not wedge the view", async () => {
    let reject: ((e: Error) => void) | null = null;
    const fn = vi.fn(() => new Promise<void>((_res, rej) => (reject = rej)));
    renderHook(() => usePoll(fn, 1000, false));

    await vi.advanceTimersByTimeAsync(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    reject!(new Error("control plane down"));
    await vi.advanceTimersByTimeAsync(1000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("leaves a void-returning fn exactly as it was — nothing to wait on, nothing skipped", () => {
    const fn = vi.fn();
    renderHook(() => usePoll(fn, 1000, false));
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it("a synchronous throw does not wedge the guard shut", () => {
    const fn = vi.fn(() => {
      throw new Error("boom");
    });
    renderHook(() => usePoll(fn, 1000, false));
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
  });
});

// R4-F073: every open tab polled at full cadence while backgrounded — the
// cockpit alone issues 122 requests/minute, and a human looking at another tab
// sees none of it.
describe("usePoll — a hidden tab", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  function setHidden(hidden: boolean) {
    vi.spyOn(document, "hidden", "get").mockReturnValue(hidden);
    document.dispatchEvent(new Event("visibilitychange"));
  }

  it("skips ticks while document.hidden, and catches up the moment the tab is looked at", () => {
    const fn = vi.fn();
    renderHook(() => usePoll(fn, 1000, false));

    vi.advanceTimersByTime(2000);
    expect(fn).toHaveBeenCalledTimes(2);

    setHidden(true);
    vi.advanceTimersByTime(10_000);
    expect(fn).toHaveBeenCalledTimes(2); // ten ticks nobody could see: none fired

    // Returning to the tab refreshes NOW rather than up to intervalMs later.
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(3);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(4);
  });

  it("stays paused-aware while hidden: unhiding a PAUSED poll fires nothing", () => {
    const fn = vi.fn();
    renderHook(({ paused }) => usePoll(fn, 1000, paused), { initialProps: { paused: true } });
    setHidden(true);
    vi.advanceTimersByTime(5000);
    setHidden(false);
    expect(fn).not.toHaveBeenCalled();
  });
});
