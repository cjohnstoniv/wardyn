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
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
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

  it("only shows egress_domain approvals — a pending credential/tool_call for this run stays out of the egress strip", async () => {
    listApprovalsMock.mockResolvedValue([
      pending({ id: "cred", kind: "credential", requested_scope: { host: "api.example", secret_name: "x" } }),
      pending({ id: "tool", kind: "tool_call", requested_scope: { cmd: "rm -rf /" } }),
    ]);
    render(<LiveApprovals runId="r1" />);
    expect(await screen.findByTestId("live-approvals-idle")).toBeInTheDocument();
    expect(screen.queryByTestId("live-approvals")).not.toBeInTheDocument();
  });

  it("approves inline via the API", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "held", requested_scope: { host: "held.example", mode: "wait_for_review" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(panel).getByRole("button", { name: /approve/i }));
    expect(approveMock).toHaveBeenCalledWith("held", expect.any(String));
  });

  // W20-W20-hold-fsm-6: a Deny click on the live strip must not go straight to
  // the API — it permanently poisons the host for the rest of the session, so
  // one misclick must be recoverable via Cancel, not just fast.
  it("Deny opens a confirm dialog and does NOT call the API until confirmed", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "a1", requested_scope: { host: "risky.example" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    expect(denyMock).not.toHaveBeenCalled();
    expect(await screen.findByText(/blocks this host for the rest of the session/i)).toBeInTheDocument();

    // Cancel backs out with no API call and no lingering dialog.
    await user.click(screen.getByRole("button", { name: /cancel/i }));
    expect(denyMock).not.toHaveBeenCalled();
    expect(screen.queryByText(/blocks this host for the rest of the session/i)).not.toBeInTheDocument();
  });

  it("confirming the Deny dialog calls the API exactly once", async () => {
    listApprovalsMock.mockResolvedValue([pending({ id: "a1", requested_scope: { host: "risky.example" } })]);
    render(<LiveApprovals runId="r1" />);
    const panel = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /^deny$/i }));

    expect(denyMock).toHaveBeenCalledTimes(1);
    expect(denyMock).toHaveBeenCalledWith("a1", expect.any(String));
  });
});
