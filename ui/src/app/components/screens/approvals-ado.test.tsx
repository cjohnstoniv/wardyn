/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /approvals list's Azure DevOps integration (plan slice S10): an
// escalation row (kind tool_call, grant_id set, requested_scope.lane
// "azure_devops") routes to AdoCapabilityCard instead of the generic
// PendingCard, an ordinary tool_call row does NOT, and the screen's own
// honesty sentence about tool_call enforcement is corrected. Its own file
// rather than more cases in approvals.test.tsx — same reason
// approvals-reauth.test.tsx gives: a different fixture, a different provider
// stack, one viewer per case.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { ADO } from "../../lib/ado-entra-copy";
import { APPROVALS } from "../../lib/approvals-copy";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

let mockRow: ApprovalRequest | null = null;
const approveMock = vi.fn();
const denyMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) => (state === "PENDING" && mockRow ? [mockRow] : []),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));

vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false } satisfies MeCapabilities),
  },
}));

vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        repo: "acme/payments-api",
        task: "checkout retry",
        confinement_class: "CC2",
        state: "RUNNING",
        created_by: "dana@acme.example",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider } from "../wardyn/operator-context";

const escalationRow: ApprovalRequest = {
  id: "apr_1",
  run_id: "run_1",
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
};

const ordinaryToolCallRow: ApprovalRequest = {
  id: "apr_3",
  run_id: "run_1",
  kind: "tool_call",
  requested_scope: { tool: "bash", cmd: "rm -rf build/" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
};

function renderScreen(operator: boolean, securityOperator: boolean, principal: string) {
  return render(
    <OperatorProvider operator={operator} securityOperator={securityOperator} principal={principal}>
      <MemoryRouter>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <ApprovalsScreen />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

beforeEach(() => {
  approveMock.mockReset();
  denyMock.mockReset();
});

describe("ApprovalsScreen — the Azure DevOps capability card", () => {
  it("the run's OWNER (a plain member, no operator tier) can decide it", async () => {
    mockRow = escalationRow;
    renderScreen(false, false, "dana@acme.example");
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    // F8 — Acts as, from the run fetch's own created_by.
    expect(card).toHaveTextContent("Acts as");
    expect(card).toHaveTextContent("dana@acme.example");
  });

  it("a security admin can decide it, regardless of ownership", async () => {
    mockRow = escalationRow;
    renderScreen(false, true, "someone-else@acme.example");
    await screen.findByTestId("ado-capability-card");
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("a member who is neither the owner nor an operator sees 'Not yours to decide' and no buttons", async () => {
    mockRow = escalationRow;
    renderScreen(false, false, "someone-else@acme.example");
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Not yours to decide"));
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("an ordinary (non-Azure-DevOps) tool_call row still falls back to the generic card", async () => {
    mockRow = ordinaryToolCallRow;
    renderScreen(true, true, "admin@acme.example");
    await screen.findByText("Tool call");
    expect(screen.queryByTestId("ado-capability-card")).not.toBeInTheDocument();
    // The generic tool_call sentence is still shown for THIS row — it is not
    // an Azure DevOps escalation, so the "stops being true" carve-out
    // doesn't apply to it.
    expect(screen.getByText(/the requesting process reads it and proceeds/)).toBeInTheDocument();
  });

  it("shows the corrected screen-level tool_call sentence once any tool_call approval is in view", async () => {
    mockRow = escalationRow;
    renderScreen(false, false, "dana@acme.example");
    await screen.findByTestId("ado-capability-card");
    expect(screen.getByText(ADO.TOOL_CALL_NOTE)).toBeInTheDocument();
  });

  // #458 — the toast on a successful decide comes from the shared canon
  // module, not a hand-typed literal.
  it("#458: approving toasts the canon APPROVALS.TOAST_APPROVED", async () => {
    mockRow = escalationRow;
    approveMock.mockResolvedValue(undefined);
    renderScreen(false, false, "dana@acme.example");
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(APPROVALS.TOAST_APPROVED));
  });

  // #458 — busy is now "approve" | "deny" | null: while Approve is deciding,
  // BOTH buttons disable and only Approve shows the spinner.
  it("#458: while Approve decides, both buttons disable and only Approve spins", async () => {
    mockRow = escalationRow;
    let resolveApprove!: () => void;
    approveMock.mockReturnValue(new Promise<void>((resolve) => { resolveApprove = resolve; }));
    renderScreen(false, false, "dana@acme.example");
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    const user = userEvent.setup();
    const approveBtn = screen.getByRole("button", { name: /approve/i });
    const denyBtn = screen.getByRole("button", { name: /deny/i });
    await user.click(approveBtn);

    await waitFor(() => expect(approveBtn).toBeDisabled());
    expect(denyBtn).toBeDisabled();
    expect(approveBtn.querySelector(".animate-spin")).toBeInTheDocument();
    expect(denyBtn.querySelector(".animate-spin")).not.toBeInTheDocument();

    resolveApprove();
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(APPROVALS.TOAST_APPROVED));
  });

  // #458 — a failed decide toasts the canon failure string, not a
  // hand-typed one.
  it("#458: a failed approve toasts APPROVALS.TOAST_APPROVE_FAILED", async () => {
    mockRow = escalationRow;
    approveMock.mockRejectedValue(new Error("boom"));
    renderScreen(false, false, "dana@acme.example");
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(APPROVALS.TOAST_APPROVE_FAILED, expect.anything()),
    );
  });
});
