/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A failed read stays on screen while a re-read is in flight: a poll tick or a Retry never flashes the skeleton.
// A tick replaces a first read that has not answered and joins a read once rows show; a remount starts from the skeleton.
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import type { RunnerView } from "../../../lib/types/runners";
import { POLL_MS, useRunnersData } from "./use-runners-data";

const listMock = vi.fn();
const settingsMock = vi.fn();
const tokensMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  runners: {
    list: (...a: unknown[]) => listMock(...a),
    settings: (...a: unknown[]) => settingsMock(...a),
    tokens: (...a: unknown[]) => tokensMock(...a),
  },
}));
vi.mock("../../../lib/api/runner-pools", () => ({ remoteRunners: { list: () => Promise.resolve([]) } }));

const LAPTOP = { id: "r-laptop-7", name: "laptop-7", state: "active", online: true } as unknown as RunnerView;

// waitFor polls on setInterval, which this test fakes, so let the real timers and promises settle instead.
const settle = () => act(async () => void (await new Promise((resolve) => setTimeout(resolve, 20))));

afterEach(() => {
  vi.useRealTimers();
  vi.resetAllMocks();
});

const pending = () => new Promise<RunnerView[]>(() => {});
const answers = () => {
  settingsMock.mockResolvedValue({ enabled: true });
  tokensMock.mockResolvedValue([]);
};

describe("useRunnersData failure hold", () => {
  it("keeps the error while a tick or a reload is in flight, until a read answers", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    settingsMock.mockResolvedValue({ enabled: true });
    tokensMock.mockResolvedValue([]);
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const { result } = renderHook(() => useRunnersData());
    await settle();
    expect(result.current.status).toBe("error");

    let resolveList: (rows: RunnerView[]) => void = () => {};
    listMock.mockReturnValueOnce(new Promise<RunnerView[]>((resolve) => (resolveList = resolve)));
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS);
    });
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("error");
    await act(async () => {
      result.current.reload();
    });
    await settle();
    expect(result.current.status).toBe("error");

    await act(async () => {
      resolveList([LAPTOP]);
    });
    await settle();
    expect(result.current.status).toBe("ready");
    expect(result.current.rows).toEqual([LAPTOP]);
  });

  it("replaces a first read that has not answered on a tick, and joins a read in flight once rows show", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    answers();
    listMock.mockReturnValueOnce(pending()).mockReturnValueOnce(Promise.resolve([LAPTOP]));
    const { result } = renderHook(() => useRunnersData());
    await settle();
    expect(listMock).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("loading");
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS);
    });
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ready");

    // Rows show: a reload starts a read, and the next tick joins it rather than starting a third.
    listMock.mockReturnValueOnce(pending());
    await act(async () => {
      result.current.reload();
    });
    await settle();
    expect(listMock).toHaveBeenCalledTimes(3);
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS);
    });
    await settle();
    expect(listMock).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("ready");
  });

  it("shows the skeleton again on a remount, not the last answer", async () => {
    answers();
    listMock.mockResolvedValueOnce([LAPTOP]).mockReturnValueOnce(pending());
    const first = renderHook(() => useRunnersData());
    await settle();
    expect(first.result.current.status).toBe("ready");
    first.unmount();
    const second = renderHook(() => useRunnersData());
    expect(second.result.current.status).toBe("loading");
    expect(second.result.current.rows).toEqual([]);
    await settle();
    expect(listMock).toHaveBeenCalledTimes(2);
  });
});
