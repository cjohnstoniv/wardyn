/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Pools tab's reads (FR-0115): a silent read that overtakes the first full load must not leave the tab
// on its loading skeleton when it fails.
import { describe, expect, it, vi } from "vitest";
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
});
