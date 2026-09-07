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
