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
