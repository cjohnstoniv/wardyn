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
