/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ReactNode } from "react";
import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OperatorProvider } from "../../wardyn/operator-context";
import { runSignIn } from "../../../lib/api/run-sign-in";
import type { RunSignIn } from "../../../lib/types";
import { SIGN_IN_GRACE_MS, SIGN_IN_POLL_MS, useRunSignIn } from "./use-run-sign-in";

vi.mock("../../../lib/api/run-sign-in", () => ({ runSignIn: { get: vi.fn() } }));
const get = vi.mocked(runSignIn.get);
const waiting: Required<RunSignIn> = { state: "waiting", user_code: "ABCD-EFGH", verification_url: "https://signin.example.test/" };
const advance = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));
let principal: string;
let resolved: boolean;

function deferred() {
  let resolve!: (value: RunSignIn) => void;
  const promise = new Promise<RunSignIn>((done) => { resolve = done; });
  return { promise, resolve };
}
function identity({ children }: { children: ReactNode }) {
  return <OperatorProvider operator={false} operatorResolved={resolved} principal={principal}>{children}</OperatorProvider>;
}
function mount() {
  const createdAt = new Date().toISOString();
  const samples: ReturnType<typeof useRunSignIn>[] = [];
  const hook = renderHook(({ runId, enabled }) => {
    const read = useRunSignIn(runId, createdAt, enabled);
    samples.push(read);
    return read;
  }, { initialProps: { runId: "run-1", enabled: true }, wrapper: identity });
  return { ...hook, samples };
}

beforeEach(() => {
  vi.useFakeTimers();
  principal = "alice";
  resolved = true;
  get.mockReset().mockResolvedValue(waiting);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("run sign-in reconciliation", () => {
  it("keeps 5-second polling and refreshes immediately on focus or visible return", async () => {
    const { result } = mount();
    await advance();
    await advance(SIGN_IN_POLL_MS - 1);
    expect(get).toHaveBeenCalledTimes(1);
    await advance(1);
    expect(get).toHaveBeenCalledTimes(2);
    act(() => { window.dispatchEvent(new Event("focus")); });
    await advance();
    expect(get).toHaveBeenCalledTimes(3);
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    act(() => { document.dispatchEvent(new Event("visibilitychange")); });
    await advance();
    expect(get).toHaveBeenCalledTimes(3);
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    act(() => { document.dispatchEvent(new Event("visibilitychange")); });
    await advance();
    expect(get).toHaveBeenCalledTimes(4);
    expect(result.current.waiting).toEqual(waiting);
    vi.restoreAllMocks();
  });

  it("coalesces focus/visibility while pending; the stale code cannot restore a completed sign-in", async () => {
    const old = deferred();
    get.mockResolvedValueOnce(waiting).mockImplementationOnce(() => old.promise).mockResolvedValue({ state: "not_waiting" });
    const { result } = mount();
    await advance();
    await advance(SIGN_IN_POLL_MS);
    act(() => {
      window.dispatchEvent(new Event("focus"));
      document.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    });
    await advance(SIGN_IN_POLL_MS * 3);
    expect(get).toHaveBeenCalledTimes(2);
    expect(get.mock.calls[1][1]?.aborted).toBe(true);
    old.resolve({ ...waiting, user_code: "STALE-CODE" });
    await advance();
    expect(get).toHaveBeenCalledTimes(3);
    expect(result.current.waiting).toBeNull();
    expect(result.current.ended).toBe(true);
    act(() => { window.dispatchEvent(new Event("focus")); });
    await advance(30_000);
    expect(get).toHaveBeenCalledTimes(3);
  });

  it("keeps the identical waiting object and leaves focus untouched", async () => {
    get.mockImplementation(() => Promise.resolve({ ...waiting }));
    const { result } = mount();
    await advance();
    const first = result.current.waiting;
    const focused = document.activeElement;
    await advance(SIGN_IN_POLL_MS * 4);
    expect(get).toHaveBeenCalledTimes(5);
    expect(result.current.waiting).toBe(first);
    expect(document.activeElement).toBe(focused);
    get.mockResolvedValue({ ...waiting, user_code: "NEXT-CODE" });
    act(() => { window.dispatchEvent(new Event("focus")); });
    await advance();
    expect(result.current.waiting).not.toBe(first);
    expect(result.current.waiting?.user_code).toBe("NEXT-CODE");
  });

  it.each(["run", "principal", "enabled"])("changing %s hides the previous code on the first render and cancels its pending read", async (change) => {
    const old = deferred();
    const fresh = deferred();
    get.mockResolvedValueOnce(waiting).mockImplementationOnce(() => old.promise).mockImplementationOnce(() => fresh.promise);
    const { result, rerender, samples } = mount();
    await advance();
    await advance(SIGN_IN_POLL_MS);
    const before = samples.length;
    if (change === "principal") principal = "bob";
    rerender({ runId: change === "run" ? "run-2" : "run-1", enabled: change !== "enabled" });
    expect(samples.slice(before).every((s) => s.waiting === null && !s.ended && !s.failed && !s.checking)).toBe(true);
    expect(get.mock.calls[1][1]?.aborted).toBe(true);
    old.resolve({ ...waiting, user_code: "STALE-CODE" });
    await advance();
    expect(result.current.waiting).toBeNull();
    if (change === "enabled") {
      expect(get).toHaveBeenCalledTimes(2);
    } else {
      expect(get).toHaveBeenCalledTimes(3);
      fresh.resolve({ ...waiting, user_code: "NEW-CODE" });
      await advance();
      expect(result.current.waiting?.user_code).toBe("NEW-CODE");
    }
  });

  it("a new run resets seen-waiting/ended and receives its own startup grace and checking delay", async () => {
    get.mockResolvedValueOnce(waiting).mockResolvedValue({ state: "not_waiting" });
    const { result, rerender } = mount();
    await advance(SIGN_IN_POLL_MS);
    expect(result.current.ended).toBe(true);
    const fresh = deferred();
    get.mockImplementationOnce(() => fresh.promise);
    rerender({ runId: "run-2", enabled: true });
    expect(result.current.ended).toBe(false);
    await advance(999);
    expect(result.current.checking).toBe(false);
    await advance(1);
    expect(result.current.checking).toBe(true);
    fresh.resolve({ state: "not_waiting" });
    await advance();
    expect(result.current.checking).toBe(false);
    expect(result.current.ended).toBe(false);
    const reads = get.mock.calls.length;
    await advance(SIGN_IN_POLL_MS);
    expect(get).toHaveBeenCalledTimes(reads + 1);
    await advance(SIGN_IN_GRACE_MS);
    const finished = get.mock.calls.length;
    await advance(SIGN_IN_POLL_MS * 2);
    expect(get).toHaveBeenCalledTimes(finished);
  });

  it("failure keeps the code and requires explicit Retry; focus cannot restart a failed read", async () => {
    get.mockResolvedValueOnce(waiting).mockRejectedValueOnce(new Error("offline")).mockResolvedValue({ state: "not_waiting" });
    const { result } = mount();
    await advance(SIGN_IN_POLL_MS);
    expect(result.current.failed).toBe(true);
    expect(result.current.waiting).toEqual(waiting);
    act(() => {
      window.dispatchEvent(new Event("focus"));
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await advance(20_000);
    expect(get).toHaveBeenCalledTimes(2);
    act(() => result.current.retry());
    await advance();
    expect(result.current.failed).toBe(false);
    expect(result.current.waiting).toBeNull();
    expect(result.current.ended).toBe(true);
  });

  it("an unresolved identity starts no read, and resolution has a fresh first-check delay", async () => {
    resolved = false;
    const fresh = deferred();
    get.mockImplementationOnce(() => fresh.promise);
    const { result, rerender } = mount();
    await advance(5000);
    expect(get).not.toHaveBeenCalled();
    expect(result.current.checking).toBe(false);
    resolved = true;
    rerender({ runId: "run-1", enabled: true });
    await advance(999);
    expect(result.current.checking).toBe(false);
    await advance(1);
    expect(result.current.checking).toBe(true);
  });

  it("unmount removes timers and listeners and ignores a pending answer", async () => {
    const old = deferred();
    get.mockImplementationOnce(() => old.promise);
    const { unmount } = mount();
    act(() => { window.dispatchEvent(new Event("focus")); });
    unmount();
    old.resolve(waiting);
    window.dispatchEvent(new Event("focus"));
    document.dispatchEvent(new Event("visibilitychange"));
    await advance(30_000);
    expect(get).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });
});
