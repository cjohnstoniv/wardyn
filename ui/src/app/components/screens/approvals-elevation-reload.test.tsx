/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your requests" on Approvals is read on mount, on every poll and after a withdraw. A read that answers late must
// not put back what a newer read replaced, and a poll slower than its interval must not stack another read.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { GovernanceRequest, GovernanceRequestList } from "../../lib/types/governance-requests";
import { aheadByHours } from "../../lib/test-clock";
import { ELEV } from "../wardyn/copy/elevation";
import { IMG } from "../wardyn/copy/image-request";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
const pendingMock = vi.fn(async () => []);
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string) => (state === "PENDING" ? pendingMock() : Promise.resolve([])),
    knowsDecidableBy: async () => false,
    listDecidableByMe: async () => ({ rows: [], truncated: false }),
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
const elevationListMock = vi.fn<() => Promise<GovernanceRequestList | null>>();
const rejectMock = vi.fn(async () => undefined);
vi.mock("../../lib/api/governance-requests", () => ({
  requests: { list: () => elevationListMock(), reject: () => rejectMock(), approve: vi.fn(), create: vi.fn() },
}));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 }),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { ApprovalsScreen } from "./approvals";

function row(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "c1",
    target_kind: "governance_elevation",
    op: "create",
    target_key: "u1",
    state: "pending",
    kind: "elevation",
    unit_id: "u1",
    unit_name: "Payments",
    duration: "4h",
    proposed_by: "me@acme.example",
    proposed_at: aheadByHours(-0.05),
    expires_at: aheadByHours(24),
    diff: { before: null, after: null, changed: [] },
    mine: true,
    decidable: false,
    deciders: [{ kind: "security" }],
    ...over,
  } as GovernanceRequest;
}
const list = (r: GovernanceRequest): GovernanceRequestList => ({
  requests: [r],
  offered: [{ unit_id: "u1", unit_name: "Payments", max_elevation: "4h" }],
  image_offered: false,
});
const PENDING = list(row());
const WITHDRAWN = list(row({ state: "rejected", decided_by: "me@acme.example", decided_at: aheadByHours(-0.01) }));

const flush = (ms = 0) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
async function mount() {
  render(
    <MemoryRouter initialEntries={["/approvals"]}>
      <ApprovalsScreen />
    </MemoryRouter>,
  );
  await flush();
}

beforeEach(() => {
  vi.useFakeTimers();
  pendingMock.mockClear();
  rejectMock.mockClear();
  elevationListMock.mockReset();
});
afterEach(() => vi.useRealTimers());

describe("ApprovalsScreen: your requests read again", () => {
  it("a poll that read the request as pending and answers after the withdraw's reload does not put it back", async () => {
    let lateAnswer: (l: GovernanceRequestList) => void = () => {};
    elevationListMock
      .mockResolvedValueOnce(PENDING)
      .mockReturnValueOnce(new Promise((done) => (lateAnswer = done)))
      .mockResolvedValueOnce(WITHDRAWN);
    await mount();
    expect(screen.getByText(ELEV.STATE_WAITING)).toBeInTheDocument();
    await flush(10_000);
    expect(elevationListMock).toHaveBeenCalledTimes(2);

    fireEvent.click(screen.getByRole("button", { name: ELEV.WITHDRAW }));
    await flush();
    expect(rejectMock).toHaveBeenCalledTimes(1);
    expect(elevationListMock).toHaveBeenCalledTimes(3);
    expect(screen.getByText(IMG.STATE_WITHDRAWN)).toBeInTheDocument();

    await act(async () => lateAnswer(PENDING));
    await flush();
    expect(screen.getByText(IMG.STATE_WITHDRAWN)).toBeInTheDocument();
    expect(screen.queryByText(ELEV.STATE_WAITING)).toBeNull();
    expect(screen.queryByRole("button", { name: ELEV.WITHDRAW })).toBeNull();
  });

  it("a poll slower than its interval is waited for, not stacked", async () => {
    elevationListMock.mockResolvedValueOnce(PENDING).mockReturnValue(new Promise(() => {}));
    await mount();
    await flush(10_000);
    expect(elevationListMock).toHaveBeenCalledTimes(2);
    const pendingReads = pendingMock.mock.calls.length;
    await flush(10_000);
    await flush(10_000);
    expect(elevationListMock).toHaveBeenCalledTimes(2);
    expect(pendingMock.mock.calls.length).toBe(pendingReads);
  });
});
