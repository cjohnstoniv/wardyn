/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Pools tab keeps today's states through TanStack Query: a failed silent read keeps the list, Retry shows loading
// until it answers, and the first failure on a tab with no pools stays on screen until a read answers or Retry fails.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { HttpError } from "../../../../lib/api/core";
import type { RunnerPool, RunnerPoolCatalogueEntry } from "../../../../lib/types/runner-pools";

const listMock = vi.fn();
const orgDefaultsMock = vi.fn();
const getMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    list: (...a: unknown[]) => listMock(...a),
    orgDefaults: (...a: unknown[]) => orgDefaultsMock(...a),
    get: (...a: unknown[]) => getMock(...a),
  },
}));

import { usePoolsData } from "./use-pools-data";

const ENTRY = { id: "p-1", name: "Staging", scope: "org" } as unknown as RunnerPoolCatalogueEntry;
const POOL = { id: "p-1", name: "Staging", scope: "org", state: "enabled", revision: 1 } as unknown as RunnerPool;
const NO_POOL_STORAGE = "this server has no pool storage";

const settle = () => act(async () => void (await new Promise((resolve) => setTimeout(resolve, 20))));
const tick = async () => {
  await act(async () => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await settle();
};
const hung = () => {
  let resolve: (v: unknown) => void = () => {};
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};

beforeEach(() => {
  listMock.mockReset();
  orgDefaultsMock.mockReset().mockResolvedValue({ pool_id: undefined });
  getMock.mockReset().mockResolvedValue(POOL);
});
afterEach(() => vi.useRealTimers());

describe("usePoolsData failure hold", () => {
  it("a silent failure keeps the list", async () => {
    listMock.mockResolvedValueOnce({ pools: [ENTRY] }).mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("ready");
    await act(async () => result.current.reload());
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ready");
    expect(result.current.entries).toHaveLength(1);
  });

  it("Retry after an error shows loading, then the pools", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("error");
    const answer = hung();
    listMock.mockReturnValueOnce(answer.promise);
    act(() => result.current.retry());
    await settle();
    expect(result.current.status).toBe("loading");
    await act(async () => answer.resolve({ pools: [ENTRY] }));
    await settle();
    expect(result.current.status).toBe("ready");
    expect(result.current.entries).toHaveLength(1);
  });

  it("a failure during a Retry shows error", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom")).mockRejectedValueOnce(new HttpError(500, "again"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    act(() => result.current.retry());
    await settle();
    expect(result.current.status).toBe("error");
  });

  it("a 501 shows unavailable with the server's message and no entries", async () => {
    listMock.mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("unavailable");
    expect(result.current.message).toBe(NO_POOL_STORAGE);
    expect(result.current.entries).toHaveLength(0);
  });

  it("a held 501 stays unavailable, with its message, while a later tick's read is pending", async () => {
    listMock.mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    listMock.mockReturnValueOnce(new Promise(() => {}));
    await tick();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("unavailable");
    expect(result.current.message).toBe(NO_POOL_STORAGE);
  });

  it("opening a pool reads it once at once", async () => {
    listMock.mockResolvedValue({ pools: [ENTRY] });
    const { result, rerender } = renderHook(({ id }: { id: string | null }) => usePoolsData(id), { initialProps: { id: null as string | null } });
    await settle();
    expect(result.current.status).toBe("ready");
    expect(getMock).toHaveBeenCalledTimes(1);
    rerender({ id: "p-1" });
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it("a tick during a Retry with no data joins it: loading stays, no extra read, then the pools", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("error");
    const answer = hung();
    listMock.mockReturnValueOnce(answer.promise);
    act(() => result.current.retry());
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    await tick();
    expect(result.current.status).toBe("loading");
    expect(listMock).toHaveBeenCalledTimes(2);
    await act(async () => answer.resolve({ pools: [ENTRY] }));
    await settle();
    expect(result.current.status).toBe("ready");
  });

  it("two failing ticks with no data leave unavailable and its first message", async () => {
    listMock.mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    await tick();
    expect(result.current.status).toBe("unavailable");
    listMock.mockRejectedValueOnce(new Error("network"));
    await tick();
    expect(listMock).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("unavailable");
    expect(result.current.message).toBe(NO_POOL_STORAGE);
  });

  it("two ticks answering 501 and then 500 leave error, which offers Retry", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    listMock.mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    await tick();
    expect(result.current.status).toBe("error");
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    await tick();
    expect(listMock).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("error");
  });

  it("a Retry's failure is the new held error: a Retry answered 501 shows unavailable", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom")).mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("error");
    act(() => result.current.retry());
    await settle();
    expect(result.current.status).toBe("unavailable");
    expect(result.current.message).toBe(NO_POOL_STORAGE);
  });

  it("a Retry answered 501 after a hung tick shows unavailable, not the error it replaced", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => usePoolsData(null));
    await settle();
    expect(result.current.status).toBe("error");
    listMock.mockReturnValueOnce(new Promise(() => {}));
    await tick();
    expect(listMock).toHaveBeenCalledTimes(2);
    listMock.mockRejectedValueOnce(new HttpError(501, NO_POOL_STORAGE));
    act(() => result.current.retry());
    await settle();
    expect(listMock).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("unavailable");
    expect(result.current.message).toBe(NO_POOL_STORAGE);
  });
});
