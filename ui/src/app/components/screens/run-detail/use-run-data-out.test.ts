/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { runs } from "../../../lib/api/runs";
import type { RunPolicyView } from "../../../lib/types";
import { useRunDataOut } from "./use-run-data-out";

vi.mock("../../../lib/api/runs", () => ({ runs: { getPolicy: vi.fn() } }));
const getPolicy = vi.mocked(runs.getPolicy);

const view = (over: Partial<RunPolicyView>): RunPolicyView =>
  ({ run_id: "run-1", state: "recorded", source: { kind: "stored" }, redacted: false, changes: [], complete: true, ...over }) as RunPolicyView;

function deferred() {
  let resolve!: (v: RunPolicyView) => void;
  const promise = new Promise<RunPolicyView>((ok) => {
    resolve = ok;
  });
  return { promise, resolve };
}

beforeEach(() => {
  getPolicy.mockReset();
});
afterEach(cleanup);

describe("useRunDataOut", () => {
  it.each(["review", "blocked", "allowed"] as const)("reads a recorded %s", async (rule) => {
    getPolicy.mockResolvedValue(view({ spec: { data_out: rule } as RunPolicyView["spec"] }));
    const { result } = renderHook(() => useRunDataOut("run-1"));
    await waitFor(() => expect(result.current).toBe(rule));
    expect(getPolicy).toHaveBeenCalledTimes(1);
    expect(getPolicy).toHaveBeenCalledWith("run-1");
  });

  it("reads a recorded spec with no data_out as allowed", async () => {
    getPolicy.mockResolvedValue(view({ spec: {} as RunPolicyView["spec"] }));
    const { result } = renderHook(() => useRunDataOut("run-1"));
    await waitFor(() => expect(result.current).toBe("allowed"));
  });

  it("is unknown while the read is out", async () => {
    const read = deferred();
    getPolicy.mockReturnValue(read.promise);
    const { result } = renderHook(() => useRunDataOut("run-1"));
    expect(result.current).toBeUndefined();
    read.resolve(view({ spec: { data_out: "blocked" } as RunPolicyView["spec"] }));
    await waitFor(() => expect(result.current).toBe("blocked"));
  });

  it.each(["not_yet", "never"] as const)("is unknown for a %s policy", async (state) => {
    const read = deferred();
    getPolicy.mockReturnValue(read.promise);
    const { result } = renderHook(() => useRunDataOut("run-1"));
    read.resolve(view({ state, spec: { data_out: "blocked" } as RunPolicyView["spec"] }));
    await act(async () => {
      await read.promise;
    });
    expect(getPolicy).toHaveBeenCalledTimes(1);
    expect(result.current).toBeUndefined();
  });

  it("is unknown when the read fails", async () => {
    getPolicy.mockImplementation(() => Promise.reject(new Error("boom")));
    const { result } = renderHook(() => useRunDataOut("run-1"));
    await act(async () => {});
    expect(getPolicy).toHaveBeenCalledTimes(1);
    expect(result.current).toBeUndefined();
  });

  it("ignores redacted: a redacted read still carries the rule", async () => {
    getPolicy.mockResolvedValue(view({ redacted: true, spec: { data_out: "blocked" } as RunPolicyView["spec"] }));
    const { result } = renderHook(() => useRunDataOut("run-1"));
    await waitFor(() => expect(result.current).toBe("blocked"));
  });

  it("reads nothing when it is not wanted", async () => {
    const { result } = renderHook(() => useRunDataOut("run-1", false));
    expect(result.current).toBeUndefined();
    expect(getPolicy).not.toHaveBeenCalled();
  });
});
