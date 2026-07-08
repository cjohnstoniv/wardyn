/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest } from "../../lib/types";

const listApprovalsMock = vi.fn((..._a: unknown[]): Promise<ApprovalRequest[]> => Promise.resolve([]));
const approveMock = vi.fn((..._a: unknown[]): Promise<unknown> => Promise.resolve({}));
const denyMock = vi.fn((..._a: unknown[]): Promise<unknown> => Promise.resolve({}));
vi.mock("../../lib/api", () => ({
  api: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { LiveApprovals } from "./live-approvals";

function pending(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "a1",
    run_id: "r1",
    kind: "egress_domain",
    requested_scope: { host: "unlisted.example" },
    state: "PENDING",
    requested_at: "",
    ...over,
  } as ApprovalRequest;
}

describe("LiveApprovals", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([]);
    approveMock.mockReset().mockResolvedValue({});
    denyMock.mockReset().mockResolvedValue({});
  });

  it("shows the idle hint when nothing is pending", async () => {
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-idle")).toBeInTheDocument();
  });

  it("flags a wait_for_review request as HELD ('waiting') and only this run's approvals", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({ id: "held", requested_scope: { host: "held.example", mode: "wait_for_review" } }),
      pending({ id: "other", run_id: "other-run", requested_scope: { host: "other.example" } }),
    ]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("held.example")).toBeInTheDocument();
    expect(within(panel).queryByText("other.example")).not.toBeInTheDocument(); // filtered by runId
    expect(within(panel).getByText("waiting")).toBeInTheDocument(); // per-row held badge (exact)
    expect(within(panel).getByText(/Sandbox is waiting/i)).toBeInTheDocument(); // header
  });

  it("approves inline via the API", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example", mode: "wait_for_review" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(panel).getByRole("button", { name: /approve/i }));
    expect(approveMock).toHaveBeenCalledWith("held", expect.any(String));
  });
});
