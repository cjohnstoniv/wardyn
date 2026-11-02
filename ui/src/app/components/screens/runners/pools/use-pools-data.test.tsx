/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Pools tab's reads (FR-0115): a silent read that overtakes the first full load must not leave the tab
// on its loading skeleton when it fails.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";

const listMock = vi.fn();
const orgDefaultsMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    list: (...a: unknown[]) => listMock(...a),
    orgDefaults: (...a: unknown[]) => orgDefaultsMock(...a),
    get: vi.fn(),
  },
}));

import { PollPauseContext } from "../../../../lib/use-poll";
import { usePoolsData } from "./use-pools-data";

describe("usePoolsData", () => {
  it("a failing silent read that overtook an unfinished full load turns the tab to error, which offers Retry", async () => {
    listMock.mockReturnValueOnce(new Promise(() => {})).mockRejectedValueOnce(new Error("network"));
    orgDefaultsMock.mockResolvedValue({ pool_id: null });
    const { result } = renderHook(() => usePoolsData(null));
    expect(result.current.status).toBe("loading");
    await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
    await waitFor(() => expect(result.current.status).toBe("error"));
  });

  describe("polling", () => {
    afterEach(() => vi.useRealTimers());

    it("re-reads every 30 s, and stops while the console is signed out", async () => {
      vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
      listMock.mockReset().mockResolvedValue({ pools: [] });
      orgDefaultsMock.mockReset().mockResolvedValue({ pool_id: null });
      let signedOut = false;
      const wrapper = ({ children }: { children: React.ReactNode }) => (
        <PollPauseContext.Provider value={signedOut}>{children}</PollPauseContext.Provider>
      );
      const { result, rerender } = renderHook(() => usePoolsData(null), { wrapper });
      await act(async () => {});
      expect(result.current.status).toBe("ready");
      expect(listMock).toHaveBeenCalledTimes(1);
      await act(async () => { vi.advanceTimersByTime(30_000); });
      expect(listMock).toHaveBeenCalledTimes(2);
      signedOut = true;
      rerender();
      await act(async () => { vi.advanceTimersByTime(90_000); });
      expect(listMock).toHaveBeenCalledTimes(2);
    });

    it("drops a tick while the previous read is still in flight", async () => {
      vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
      listMock.mockReset().mockReturnValue(new Promise(() => {}));
      orgDefaultsMock.mockReset().mockResolvedValue({ pool_id: null });
      renderHook(() => usePoolsData(null));
      await act(async () => { vi.advanceTimersByTime(30_000); });
      await act(async () => { vi.advanceTimersByTime(90_000); });
      // The first tick starts one read; the rest wait for it.
      expect(listMock).toHaveBeenCalledTimes(2);
    });
  });
});
