/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useCopyToClipboard } from "./use-copy-to-clipboard";

describe("useCopyToClipboard", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("copy() flips copied immediately and auto-resets after resetMs", () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn() } });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    act(() => result.current.copy("hi"));
    expect(result.current.copied).toBe(true);

    act(() => vi.advanceTimersByTime(1000));
    expect(result.current.copied).toBe(false);
  });

  it("copyAsync() only flips copied on a resolved write, and resolves false on rejection", async () => {
    const write = vi.fn().mockRejectedValueOnce(new Error("denied")).mockResolvedValueOnce(undefined);
    Object.assign(navigator, { clipboard: { writeText: write } });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    let ok = true;
    await act(async () => {
      ok = await result.current.copyAsync("x");
    });
    expect(ok).toBe(false);
    expect(result.current.copied).toBe(false);

    await act(async () => {
      ok = await result.current.copyAsync("x");
    });
    expect(ok).toBe(true);
    expect(result.current.copied).toBe(true);
  });

  it("with resetMs=null, copied stays true until reset externally", () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn() } });
    const { result } = renderHook(() => useCopyToClipboard(null));

    act(() => result.current.copy("hi"));
    expect(result.current.copied).toBe(true);

    act(() => vi.advanceTimersByTime(10_000));
    expect(result.current.copied).toBe(true);

    act(() => result.current.setCopied(false));
    expect(result.current.copied).toBe(false);
  });
});
