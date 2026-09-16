/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { toast } from "sonner";
import { useCopyToClipboard } from "./use-copy-to-clipboard";

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

describe("useCopyToClipboard", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.mocked(toast.error).mockClear();
  });
  afterEach(() => vi.useRealTimers());

  it("copy() flips copied once the write resolves, and auto-resets after resetMs", async () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    await act(async () => {
      result.current.copy("hi");
      await Promise.resolve(); // let the fire-and-forget copyAsync() microtask settle
    });
    expect(result.current.copied).toBe(true);

    act(() => vi.advanceTimersByTime(1000));
    expect(result.current.copied).toBe(false);
  });

  it("copy() never flips copied when navigator.clipboard is unavailable (no false 'Copied' on LAN HTTP)", async () => {
    Object.assign(navigator, { clipboard: undefined });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    await act(async () => {
      result.current.copy("hi");
      await Promise.resolve();
    });
    expect(result.current.copied).toBe(false);
  });

  // F6-F13: copy() used to be fire-and-forget and silently discard the
  // outcome — a member on LAN HTTP (an insecure context: no
  // navigator.clipboard) clicked Copy and nothing ever told them it didn't
  // work. copyAsync already returns the outcome; copy() must surface it.
  it("F6-F13: copy() surfaces a failure toast when navigator.clipboard is unavailable", async () => {
    Object.assign(navigator, { clipboard: undefined });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    await act(async () => {
      await result.current.copy("hi");
    });
    expect(toast.error).toHaveBeenCalledTimes(1);
  });

  // neg: a successful copy must never toast — the icon swap is the whole
  // confirmation for the happy path, unchanged since before this fix.
  it("neg: copy() does not toast on a successful write", async () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
    const { result } = renderHook(() => useCopyToClipboard(1000));

    await act(async () => {
      await result.current.copy("hi");
    });
    expect(toast.error).not.toHaveBeenCalled();
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

  it("with resetMs=null, copied stays true until reset externally", async () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
    const { result } = renderHook(() => useCopyToClipboard(null));

    await act(async () => {
      result.current.copy("hi");
      await Promise.resolve();
    });
    expect(result.current.copied).toBe(true);

    act(() => vi.advanceTimersByTime(10_000));
    expect(result.current.copied).toBe(true);

    act(() => result.current.setCopied(false));
    expect(result.current.copied).toBe(false);
  });
});
