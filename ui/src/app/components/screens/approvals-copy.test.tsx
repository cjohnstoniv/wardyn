/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

// MEDIUM fixes pinned here:
//  - the approve dialog must say "Approve" (NOT "Approve & mint") — approval
//    authorizes the broker to mint later, it does not mint.
//  - EXPIRED approvals must be fetched and shown in the decided view.
// (The 10s poll interval added alongside these fixes never fires within a test,
//  so no fake timers are needed.)

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

const listApprovalsMock = vi.fn();
vi.mock("../../lib/api", () => ({
  api: {
    listApprovals: (state: string) => listApprovalsMock(state),
    approve: vi.fn().mockResolvedValue({}),
    deny: vi.fn().mockResolvedValue({}),
    // RunContextRow (redesign) fetches the gated run to inline its context.
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        repo: "acme/widgets",
        task: "Fix flaky auth tests",
        confinement_class: "CC2",
        state: "RUNNING",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";

const pending: ApprovalRequest = {
  id: "apr_pending",
  run_id: "run_1",
  kind: "credential",
  requested_scope: { host: "api.example.com" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
};
const expired: ApprovalRequest = {
  id: "apr_expired",
  run_id: "run_expired_42",
  kind: "credential",
  requested_scope: { host: "expired.example.com" },
  state: "EXPIRED",
  requested_at: new Date(Date.now() - 60_000).toISOString(),
};

describe("ApprovalsScreen — copy + EXPIRED inclusion", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset();
    listApprovalsMock.mockImplementation((state: string) => {
      if (state === "PENDING") return Promise.resolve([pending]);
      if (state === "EXPIRED") return Promise.resolve([expired]);
      return Promise.resolve([]); // APPROVED, DENIED
    });
  });

  it("fetches EXPIRED approvals on load", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    await waitFor(() => expect(listApprovalsMock).toHaveBeenCalledWith("EXPIRED"));
  });

  it("shows EXPIRED requests in the decided view", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    // Wait for initial load, then switch to the Decided tab.
    const decidedTab = await screen.findByRole("tab", { name: /decided/i });
    await user.click(decidedTab);
    // The EXPIRED request's run id must render in the decided list.
    await waitFor(() => expect(screen.getByText(/run_expired_42/)).toBeInTheDocument());
  });

  it("labels the approve dialog 'Approve', not 'Approve & mint'", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const approveBtn = await screen.findByRole("button", { name: /^approve$/i });
    await user.click(approveBtn);
    // The dialog opens; its confirm button reads exactly "Approve".
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: /^approve$/i }).length).toBeGreaterThan(0),
    );
    // Nothing on screen may over-claim "& mint".
    expect(screen.queryByText(/approve & mint/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/& mint/i)).not.toBeInTheDocument();
  });
});
