/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pending queue, Waiting on you and the decided history are read on mount, on every poll and after a decision. A
// poll read that answers after a newer read must not put back what that read replaced, and a read that answers while
// a newer one is still out is shown: a list is never drawn empty and ready on a read that was thrown away.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
let knows = false;
const pendingMock = vi.fn<() => Promise<ApprovalRequest[]>>();
const decidedMock = vi.fn<(state: string) => Promise<ApprovalRequest[]>>();
const waitingMock = vi.fn<() => Promise<{ rows: ApprovalRequest[]; truncated: boolean }>>();
const approveMock = vi.fn(async () => undefined);
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string) => (state === "PENDING" ? pendingMock() : decidedMock(state)),
    knowsDecidableBy: async () => knows,
    listDecidableByMe: () => waitingMock(),
    approve: () => approveMock(),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/governance-requests", () => ({ requests: { list: async () => null } }));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 }),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider, RoleProvider } from "../wardyn/operator-context";

const NOW = new Date().toISOString();
const egress = (id: string, extra: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "egress_domain",
  requested_scope: { host: `${id}.example.com` },
  state: "PENDING",
  requested_at: NOW,
  ...extra,
});
const later = <T,>() => {
  let answer: (v: T) => void = () => {};
  const promise = new Promise<T>((done) => (answer = done));
  return { promise, answer };
};

const flush = (ms = 0) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
async function mount(url = "/approvals", member = false) {
  const screenAt = (
    <MemoryRouter initialEntries={[url]}>
      <ApprovalsScreen />
    </MemoryRouter>
  );
  render(
    member ? (
      <OperatorProvider operator={false} securityOperator={false}>
        <RoleProvider role="user">{screenAt}</RoleProvider>
      </OperatorProvider>
    ) : (
      screenAt
    ),
  );
  await flush();
}
async function approveInDialog() {
  fireEvent.click(screen.getByRole("button", { name: /^approve$/i }));
  await flush();
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^approve$/i }));
  await flush();
  expect(approveMock).toHaveBeenCalledTimes(1);
}

beforeEach(() => {
  vi.useFakeTimers();
  knows = false;
  pendingMock.mockReset().mockResolvedValue([]);
  decidedMock.mockReset().mockResolvedValue([]);
  waitingMock.mockReset().mockResolvedValue({ rows: [], truncated: false });
  approveMock.mockClear();
});
afterEach(() => vi.useRealTimers());

describe("ApprovalsScreen: a list read again after a decision", () => {
  it("a poll read that saw the request pending and answers after the decision's refresh does not put it back", async () => {
    const poll = later<ApprovalRequest[]>();
    pendingMock.mockResolvedValueOnce([egress("p1")]).mockReturnValueOnce(poll.promise);
    await mount();
    expect(screen.getByText("Reach p1.example.com")).toBeInTheDocument();
    await flush(10_000);
    expect(pendingMock).toHaveBeenCalledTimes(2);

    await approveInDialog();
    expect(pendingMock).toHaveBeenCalledTimes(3);
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();

    await act(async () => poll.answer([egress("p1")]));
    await flush();
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();
    expect(screen.queryByRole("button", { name: /^approve$/i })).toBeNull();
  });

  it("on Waiting on you, a poll read that answers after the decision's refresh does not put the request back", async () => {
    knows = true;
    const row = egress("w1", { requested_by: "dana.reyes@acme.example", deciders: ["owner", "unit_admins", "security"] });
    const poll = later<{ rows: ApprovalRequest[]; truncated: boolean }>();
    waitingMock.mockResolvedValueOnce({ rows: [row], truncated: false }).mockReturnValueOnce(poll.promise);
    await mount("/approvals?tab=waiting", true);
    expect(screen.getByText("Reach w1.example.com")).toBeInTheDocument();
    await flush(10_000);
    expect(waitingMock).toHaveBeenCalledTimes(2);

    await approveInDialog();
    expect(waitingMock).toHaveBeenCalledTimes(3);
    expect(screen.queryByText("Reach w1.example.com")).toBeNull();

    await act(async () => poll.answer({ rows: [row], truncated: false }));
    await flush();
    expect(screen.queryByText("Reach w1.example.com")).toBeNull();
  });

  it("a poll read of the decided history that answers after a newer read does not drop what that read added", async () => {
    const poll = later<ApprovalRequest[]>();
    const decided = egress("d1", { state: "APPROVED", decided_by: "dana.reyes@acme.example", decided_at: NOW });
    let approvedReads = 0;
    decidedMock.mockImplementation((state: string) => {
      if (state !== "APPROVED") return Promise.resolve([]);
      approvedReads += 1;
      return approvedReads === 2 ? poll.promise : Promise.resolve(approvedReads === 1 ? [] : [decided]);
    });
    await mount("/approvals?tab=decided");
    await flush(10_000);
    expect(approvedReads).toBe(2);

    fireEvent.mouseDown(screen.getByRole("tab", { name: /pending/i }), { button: 0 });
    await flush();
    fireEvent.mouseDown(screen.getByRole("tab", { name: /decided/i }), { button: 0 });
    await flush();
    expect(approvedReads).toBe(3);
    expect(screen.getByText("Reach d1.example.com")).toBeInTheDocument();

    await act(async () => poll.answer([]));
    await flush();
    expect(screen.getByText("Reach d1.example.com")).toBeInTheDocument();
  });
});

describe("ApprovalsScreen: a read that answers while a newer one is still out", () => {
  it("the pending queue shows a poll read that answers while Retry's read is out, not an empty queue", async () => {
    const poll = later<ApprovalRequest[]>();
    pendingMock.mockReset().mockRejectedValueOnce(new Error("down")).mockReturnValueOnce(poll.promise).mockReturnValueOnce(new Promise(() => {}));
    await mount();
    await flush(10_000);
    expect(pendingMock).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await flush();
    expect(pendingMock).toHaveBeenCalledTimes(3);

    await act(async () => poll.answer([egress("p1")]));
    await flush();
    expect(screen.getByText("Reach p1.example.com")).toBeInTheDocument();
    expect(screen.queryByText("No pending approvals")).toBeNull();
  });

  it("Waiting on you draws its tab from a first read that answers while a poll read is out", async () => {
    knows = true;
    const row = egress("w1", { requested_by: "dana.reyes@acme.example", deciders: ["owner", "unit_admins", "security"] });
    const first = later<{ rows: ApprovalRequest[]; truncated: boolean }>();
    waitingMock.mockReset().mockReturnValueOnce(first.promise).mockReturnValueOnce(new Promise(() => {}));
    await mount("/approvals?tab=waiting", true);
    await flush(10_000);
    expect(waitingMock).toHaveBeenCalledTimes(2);

    await act(async () => first.answer({ rows: [row], truncated: false }));
    await flush();
    expect(screen.getByText("Reach w1.example.com")).toBeInTheDocument();
  });

  it("the decided history shows its first read when a poll read started before it answered, not an empty history", async () => {
    const first = later<ApprovalRequest[]>();
    const decided = egress("d1", { state: "APPROVED", decided_by: "dana.reyes@acme.example", decided_at: NOW });
    let approvedReads = 0;
    decidedMock.mockImplementation((state: string) => {
      if (state !== "APPROVED") return Promise.resolve([]);
      approvedReads += 1;
      return approvedReads === 1 ? first.promise : new Promise(() => {});
    });
    await mount();
    await flush(9_000);
    fireEvent.mouseDown(screen.getByRole("tab", { name: /decided/i }), { button: 0 });
    await flush();
    expect(approvedReads).toBe(1);
    await flush(1_000);
    expect(approvedReads).toBe(2);

    await act(async () => first.answer([decided]));
    await flush();
    expect(screen.getByText("Reach d1.example.com")).toBeInTheDocument();
    expect(screen.queryByText("No decisions yet")).toBeNull();
  });
});
