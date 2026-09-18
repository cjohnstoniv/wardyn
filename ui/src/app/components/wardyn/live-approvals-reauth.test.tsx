/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The mid-run AWS sign-in row (Finding 4). Its own file rather than more cases
// in live-approvals.test.tsx (643 lines): this row has a different shape — a
// door, not a decision — a different audience rule, and its own provider.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { ModelAccessProvider } from "./model-access-context";
import { SECURITY_ONLY_REASON } from "./copy";
import { REAUTH_ROW, REAUTH_HEADING, REAUTH_SIGNED_IN_TOAST } from "./model-access-copy";

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
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: (...a: unknown[]) => toastSuccess(...a), info: vi.fn() } }));

import { LiveApprovals, isHeld } from "./live-approvals";

function reauthRow(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "reauth-1",
    run_id: "r1",
    kind: "credential_reauth",
    requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner: "alice@corp" },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest;
}

function mount(operator: boolean) {
  return render(
    <OperatorProvider operator={operator} securityOperator={operator}>
      <ModelAccessProvider status={null} onRefresh={() => {}}>
        <LiveApprovals runId="r1" />
      </ModelAccessProvider>
    </OperatorProvider>,
  );
}

describe("LiveApprovals — the mid-run AWS sign-in row", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([]);
    approveMock.mockReset().mockResolvedValue({});
    denyMock.mockReset().mockResolvedValue({});
    toastSuccess.mockReset();
  });

  it("is HELD by construction — it exists because the proxy is holding a call", () => {
    // It carries no first_use mode of its own (that vocabulary is the egress
    // lane's), so without the kind arm it would read as a passive pending and
    // the run would show no hold while a model call was parked.
    expect(isHeld(reauthRow())).toBe(true);
  });

  it("renders a DOOR, not a decision: no Approve, no Deny, one named button", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(true); // even a SECURITY OPERATOR gets no pair — no tier can decide it
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(REAUTH_ROW.label)).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: /^Deny$/ })).not.toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
  });

  it("names the need in the heading, not a decision nobody makes", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(false);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(REAUTH_HEADING)).toBeInTheDocument();
    expect(within(panel).queryByText(/approve to let it through/i)).not.toBeInTheDocument();
  });

  it("does not tell a member an admin decides it — no tier does", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(false);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
  });

  it("promises nothing about the run — the row is not proof a call is still parked", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(false);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(REAUTH_ROW.hint)).toBeInTheDocument();
    // Both branches, because the console cannot tell which one the person is in.
    expect(REAUTH_ROW.hint).toMatch(/if it already failed, relaunch it/i);
  });

  it("the button DECIDES nothing — it opens the door", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(false);
    const panel = await screen.findByTestId("live-approvals");
    await userEvent.click(within(panel).getByRole("button", { name: REAUTH_ROW.ariaLabel }));
    // The ONE dialog instance is context-owned and rendered by the shell, not
    // by this row (app-shell-model-access.test.tsx pins the dialog itself), so
    // what this row owes is: it opens it, and it never posts a decision the
    // server would 409.
    expect(approveMock).not.toHaveBeenCalled();
    expect(denyMock).not.toHaveBeenCalled();
    expect(within(panel).getByText(REAUTH_ROW.action)).toBeInTheDocument();
  });

  it("on the SHARED lane a member gets the instruction, never a door the server refuses", async () => {
    listApprovalsMock.mockResolvedValue([
      reauthRow({ requested_scope: { mechanism: "bedrock_sso", credential_source: "shared", owner: "" } }),
    ]);
    mount(false);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(REAUTH_ROW.sharedMemberHint)).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).not.toBeInTheDocument();
  });

  it("…and the ADMIN on that same shared row gets the door", async () => {
    listApprovalsMock.mockResolvedValue([
      reauthRow({ requested_scope: { mechanism: "bedrock_sso", credential_source: "shared", owner: "" } }),
    ]);
    mount(true);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
  });

  it("toasts 'Signed in' — and nothing about the run — when the row is APPROVED", async () => {
    listApprovalsMock.mockImplementation((state: unknown) => {
      if (state === "APPROVED") return Promise.resolve([reauthRow({ state: "APPROVED" })]);
      // First PENDING read has the row; every later one does not.
      return listApprovalsMock.mock.calls.filter((c) => c[0] === "PENDING").length <= 1
        ? Promise.resolve([reauthRow()])
        : Promise.resolve([]);
    });
    mount(false);
    await screen.findByText(REAUTH_ROW.label);
    await vi.waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(REAUTH_SIGNED_IN_TOAST), { timeout: 8000 });
    // The claim it must NEVER make: APPROVED proves the sign-in landed, not
    // that the held call resumed.
    expect(String(toastSuccess.mock.calls[0]?.[0])).not.toMatch(/continu/i);
  });

  it("does NOT toast when the row simply vanished (cancelled, expired, or the run ended)", async () => {
    listApprovalsMock.mockImplementation((state: unknown) => {
      if (state === "APPROVED") return Promise.resolve([]);
      return listApprovalsMock.mock.calls.filter((c) => c[0] === "PENDING").length <= 1
        ? Promise.resolve([reauthRow()])
        : Promise.resolve([]);
    });
    mount(false);
    await screen.findByText(REAUTH_ROW.label);
    await new Promise((r) => setTimeout(r, 50));
    expect(toastSuccess).not.toHaveBeenCalled();
  });
});
