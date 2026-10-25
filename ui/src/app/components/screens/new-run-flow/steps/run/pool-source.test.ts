/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

import { loadPools, usePools, type LoadPools } from "./pool-source";

beforeEach(() => {
  wfetchMock.mockReset();
  wfetchMock.mockImplementation(async () => new Response('{"pools":[]}', { status: 200, headers: { "content-type": "application/json" } }));
});

describe("the Run step's pool read", () => {
  it.each([
    ["interactive", "/runner-pools?run_type=interactive"],
    ["background", "/runner-pools?run_type=background"],
    [undefined, "/runner-pools"],
  ] as const)("asks for run type %s", async (runType, path) => {
    await loadPools(runType, new AbortController().signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(path);
  });
});

describe("usePools", () => {
  it("a first request that rejects while the run type is undefined reports an error and does not throw", async () => {
    const load = vi.fn<LoadPools>().mockRejectedValue(new Error("network"));
    const { result } = renderHook(() => usePools(load, undefined));
    expect(result.current.status).toBe("loading");
    await waitFor(() => expect(result.current).toEqual({ status: "error", message: "network" }));
  });
});
