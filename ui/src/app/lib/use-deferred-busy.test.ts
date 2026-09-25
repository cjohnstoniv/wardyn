/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useDeferredBusy } from "./use-deferred-busy";

describe("useDeferredBusy", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("disables immediately, but the spinner waits out the delay", async () => {
    const { result, rerender } = renderHook(({ busy }) => useDeferredBusy(busy), {
      initialProps: { busy: false },
    });
    expect(result.current).toEqual({ disabled: false, showSpinner: false });

    rerender({ busy: true });
    expect(result.current.disabled).toBe(true);
    expect(result.current.showSpinner).toBe(false);

    await act(() => vi.advanceTimersByTime(199));
    expect(result.current.showSpinner).toBe(false);
    await act(() => vi.advanceTimersByTime(1));
    expect(result.current.showSpinner).toBe(true);
  });

  it("a fast action never flashes a spinner", async () => {
    const { result, rerender } = renderHook(({ busy }) => useDeferredBusy(busy), {
      initialProps: { busy: true },
    });
    await act(() => vi.advanceTimersByTime(100));
    rerender({ busy: false });
    expect(result.current.disabled).toBe(false);
    expect(result.current.showSpinner).toBe(false);

    // The pending 200ms timer from the first busy period must not fire late.
    await act(() => vi.advanceTimersByTime(200));
    expect(result.current.showSpinner).toBe(false);
  });

  it("turns the spinner off immediately when busy ends, even mid-spin", async () => {
    const { result, rerender } = renderHook(({ busy }) => useDeferredBusy(busy), {
      initialProps: { busy: true },
    });
    await act(() => vi.advanceTimersByTime(200));
    expect(result.current.showSpinner).toBe(true);

    rerender({ busy: false });
    expect(result.current.disabled).toBe(false);
    expect(result.current.showSpinner).toBe(false);
  });

  it("honors a custom delayMs", async () => {
    const { result, rerender } = renderHook(({ busy }) => useDeferredBusy(busy, 50), {
      initialProps: { busy: false },
    });
    rerender({ busy: true });
    await act(() => vi.advanceTimersByTime(49));
    expect(result.current.showSpinner).toBe(false);
    await act(() => vi.advanceTimersByTime(1));
    expect(result.current.showSpinner).toBe(true);
  });
});
