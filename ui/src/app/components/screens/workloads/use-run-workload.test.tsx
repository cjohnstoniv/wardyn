/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useRunWorkload } from "./use-run-workload";

const listMock = vi.fn();
vi.mock("../../../lib/api/workloads", () => ({ workloads: { ofRun: (...a: unknown[]) => listMock(...a) } }));

afterEach(() => vi.clearAllMocks());

describe("useRunWorkload", () => {
  it("asks for the workload of this run alone, not the list", async () => {
    listMock.mockResolvedValue({ name: "billing-api", owner: "service:billing", run_id: "r-1" });
    const { result } = renderHook(() => useRunWorkload("r-1"));
    await waitFor(() => expect(result.current).toEqual({ name: "billing-api", owner: "service:billing" }));
    expect(listMock).toHaveBeenCalledTimes(1);
    expect(listMock).toHaveBeenCalledWith("r-1");
  });

  it("is none for a run no workload owns, and when the list is refused", async () => {
    listMock.mockResolvedValueOnce(undefined);
    const { result, rerender } = renderHook(({ id }) => useRunWorkload(id), { initialProps: { id: "r-1" } });
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(1));
    expect(result.current).toBeUndefined();
    listMock.mockRejectedValueOnce(new Error("forbidden"));
    rerender({ id: "r-3" });
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
    expect(result.current).toBeUndefined();
  });

  it("asks nothing before the run is known", () => {
    renderHook(() => useRunWorkload(undefined));
    expect(listMock).not.toHaveBeenCalled();
  });
});
