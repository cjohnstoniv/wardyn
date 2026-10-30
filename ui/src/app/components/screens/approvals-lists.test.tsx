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
const listRunsMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { getRun: () => Promise.reject(new Error("unused")), listRuns: () => listRunsMock() },
}));

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
    listRunsMock.mockReset();
    listRunsMock.mockResolvedValue([]);
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

  it("#3516: a failed first load of the Decided tab heals on the next refresh, with no Retry click", async () => {
    const row = {
      id: "ap_1",
      run_id: "run_1",
      kind: "egress_domain",
      requested_scope: { host: "example.com" },
      state: "APPROVED",
      requested_at: "2026-10-20T10:00:00Z",
      decided_at: "2026-10-20T10:01:00Z",
      decided_by: "alice",
    };
    let failDecided = true;
    listMock.mockImplementation((state: string) =>
      DECIDED.includes(state) && failDecided
        ? Promise.reject(new Error("boom"))
        : Promise.resolve(state === "APPROVED" ? [row] : []),
    );
    await mountAt("/approvals?tab=decided");
    expect(screen.getByRole("button", { name: /retry/i })).toBeTruthy();
    expect(screen.queryByTestId("run-ref")).toBeNull();
    failDecided = false;
    await tick(10_000);
    expect(screen.queryByRole("button", { name: /retry/i })).toBeNull();
    expect(screen.getByTestId("run-ref")).toBeTruthy();
  });

  it("#3516: a refresh that keeps failing keeps the Decided tab's error", async () => {
    listMock.mockImplementation((state: string) =>
      DECIDED.includes(state) ? Promise.reject(new Error("boom")) : Promise.resolve([]),
    );
    await mountAt("/approvals?tab=decided");
    await tick(10_000);
    await tick(10_000);
    expect(screen.getByRole("button", { name: /retry/i })).toBeTruthy();
  });

  it("#3516: with only the Pending tab open, a refresh never fetches the decided lists", async () => {
    await mountAt("/approvals");
    await tick(10_000);
    expect(calls(...DECIDED).length).toBe(0);
  });

  it("#3036: a Decided row names its run by title and short id, not the bare id", async () => {
    const runId = "9f8e7d6c-1111-2222-3333-444455556666";
    listRunsMock.mockResolvedValue([{ id: runId, title: "Fix the flaky login test", task: "unused" }]);
    listMock.mockImplementation((state: string) =>
      Promise.resolve(
        state === "APPROVED"
          ? [
              {
                id: "ap_1",
                run_id: runId,
                kind: "egress_domain",
                requested_scope: { host: "example.com" },
                state: "APPROVED",
                requested_at: "2026-10-20T10:00:00Z",
                decided_at: "2026-10-20T10:01:00Z",
                decided_by: "alice",
              },
            ]
          : [],
      ),
    );
    await mountAt("/admin/approvals?tab=decided");
    const ref = screen.getByTestId("run-ref");
    expect(ref.textContent).toContain("Fix the flaky login test");
    expect(ref.textContent).toContain("9f8e7d6c");
  });
});
