/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run cockpit strip's Azure DevOps integration (plan slice S10). Its own
// file rather than more cases in live-approvals.test.tsx, mirroring
// live-approvals-reauth.test.tsx's own precedent: a different fixture (an
// escalation's canonical scope) and a different assertion shape (a full
// card, not a one-line row).

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { ModelAccessProvider } from "./model-access-context";

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

function escalationRow(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "esc-1",
    run_id: "r1",
    grant_id: "grant_1",
    kind: "tool_call",
    requested_scope: {
      lane: "azure_devops",
      provider_id: "row_1",
      org: "acme",
      grant_id: "grant_1",
      capability: "code_write",
      repo: "payments-api",
      ref_class: "",
      tool: "Azure DevOps",
      cmd: "Push commits and move branches that no policy protects (code_write) in acme/payments-api",
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest;
}

function mount() {
  return render(
    <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
      <MemoryRouter>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <LiveApprovals runId="r1" />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

describe("LiveApprovals — the Azure DevOps capability card, in the run cockpit", () => {
  it("renders the full card, decidable, for a plain member with no operator tier", async () => {
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount();
    const card = await screen.findByTestId("ado-capability-card");
    expect(card).toHaveTextContent("Push");
    // Decidable here regardless of the viewer's own operator tier: reaching
    // this cockpit page at all already proved run ownership or admin (GET
    // /runs/{id} -> ownsRunOrAdmin) — see the card's mount-site comment.
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("Approve sends an explicit decision_scope 'run' — never a bodyless decide", async () => {
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount();
    const card = await screen.findByTestId("ado-capability-card");
    await userEvent.click(within(card).getByRole("button", { name: "Approve" }));
    expect(approveMock).toHaveBeenCalledWith("esc-1", expect.any(String), { scope: "run" });
  });

  it("an ordinary tool_call row (no Azure DevOps lane) still renders the strip's plain one-liner", async () => {
    listApprovalsMock.mockResolvedValue([
      { id: "t1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "bash", cmd: "rm -rf build/" }, state: "PENDING", requested_at: new Date().toISOString() } as ApprovalRequest,
    ]);
    mount();
    await screen.findByTestId("live-approval-row");
    expect(screen.queryByTestId("ado-capability-card")).not.toBeInTheDocument();
  });
});
