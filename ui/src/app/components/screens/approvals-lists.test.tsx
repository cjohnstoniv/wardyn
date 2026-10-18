/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// #2486 and #2487: which lists the Approvals screen asks for, in which console view, and how often.

const listMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string, runId: string, view: string) => listMock(state, runId, view),
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { ApprovalsScreen } from "./approvals";

const DECIDED = ["APPROVED", "DENIED", "EXPIRED", "CANCELLED"];
const calls = (...states: string[]) => listMock.mock.calls.filter((c) => states.includes(c[0] as string));

async function mountAt(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <ApprovalsScreen />
    </MemoryRouter>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}
const tick = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

describe("ApprovalsScreen lists", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    listMock.mockReset();
    listMock.mockResolvedValue([]);
    return () => vi.useRealTimers();
  });

  it("#2486: /approvals asks for view=user and /admin/approvals for view=admin", async () => {
    await mountAt("/approvals");
    expect(calls("PENDING").map((c) => c[2])).toEqual(["user"]);
    listMock.mockClear();
    cleanup();
    await mountAt("/admin/approvals?tab=decided");
    expect(listMock.mock.calls.length).toBeGreaterThan(0);
    expect(listMock.mock.calls.every((c) => c[2] === "admin")).toBe(true);
  });

  it("#2487: the pending tab reads PENDING on the first load and on every tick, and never the decided history", async () => {
    await mountAt("/approvals");
    await tick(10_000);
    await tick(10_000);
    expect(calls("PENDING").length).toBe(3);
    expect(calls(...DECIDED).length).toBe(0);
  });

  it("#2487: the Decided tab reads the four decided lists on open and again on each tick", async () => {
    await mountAt("/approvals");
    // Radix Tabs select on mousedown.
    fireEvent.mouseDown(screen.getByRole("tab", { name: /decided/i }), { button: 0 });
    await tick(0);
    expect(calls(...DECIDED).map((c) => c[0]).sort()).toEqual([...DECIDED].sort());
    await tick(10_000);
    expect(calls(...DECIDED).length).toBe(8);
  });
});
