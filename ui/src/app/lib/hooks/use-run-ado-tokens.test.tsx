/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's Azure DevOps token list re-reads on an interval while the run is live, and holds while the
// console is signed out.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { PollPauseContext } from "../use-poll";

const runTokensMock = vi.fn();
vi.mock("../api/ado-pat", () => ({ adoPat: { runTokens: (id: string) => runTokensMock(id) } }));

import { useRunAdoTokens } from "./use-run-ado-tokens";

describe("useRunAdoTokens polling", () => {
  afterEach(() => vi.useRealTimers());

  it("re-reads every 10 s while live, and stops while the console is signed out", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    runTokensMock.mockReset().mockResolvedValue([]);
    let signedOut = false;
    const wrapper = ({ children }: { children: React.ReactNode }) => (
      <PollPauseContext.Provider value={signedOut}>{children}</PollPauseContext.Provider>
    );
    const { rerender } = renderHook(() => useRunAdoTokens("run-1", true), { wrapper });
    expect(runTokensMock).toHaveBeenCalledTimes(1);
    await act(async () => { vi.advanceTimersByTime(10_000); });
    expect(runTokensMock).toHaveBeenCalledTimes(2);
    signedOut = true;
    rerender();
    await act(async () => { vi.advanceTimersByTime(60_000); });
    expect(runTokensMock).toHaveBeenCalledTimes(2);
  });

  it("reads once and never again for a run that is not live", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    runTokensMock.mockReset().mockResolvedValue([]);
    renderHook(() => useRunAdoTokens("run-1", false));
    await act(async () => { vi.advanceTimersByTime(60_000); });
    expect(runTokensMock).toHaveBeenCalledTimes(1);
  });
});
