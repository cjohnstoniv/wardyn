/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The mid-run AWS sign-in row. Its own file rather than more cases in
// live-approvals.test.tsx (643 lines): this row has a different shape — a
// door, not a decision — a different audience rule, and its own provider.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { ModelAccessProvider } from "./model-access-context";
import { SECURITY_ONLY_REASON } from "./copy";
import { REAUTH_ROW, REAUTH_HEADING, REAUTH_SIGNED_IN_TOAST } from "./model-access-copy";
import { OPEN_IN_USER_VIEW } from "./copy/console-view";

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

// The viewer, named: every row below is OWNED BY alice@corp, so the default
// principal is alice's — the "owner" cell each of these cases was written for.
// A case that means a different viewer passes one: the door is the VIEWER's
// own sign-in, and only the row's owner can resolve it.
function mount(operator: boolean, principal = "alice@corp", adminView = false) {
  return render(
    <OperatorProvider operator={operator} securityOperator={operator} principal={principal}>
      <ModelAccessProvider status={null} onRefresh={() => {}}>
        <LiveApprovals runId="r1" adminView={adminView} />
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

  // The four cells of the one ownership rule: the door is the viewer's OWN
  // sign-in — a capture lands in the capturer's scope, so reauthResolvableBy
  // (internal/api's injection_awssso.go) admits only the subject the row
  // names on the per_user lane, and only the operator — whose credential the
  // shared one is — on the shared lane. The other two cells above pin the
  // shared lane; these two pin the per_user one.
  it("the OWNER gets the door, member or not — it is their own sign-in", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    mount(false, "alice@corp");
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
    expect(within(panel).getByText(REAUTH_ROW.hint)).toBeInTheDocument();
  });

  it("an ADMIN reading a MEMBER's held run gets no door and a sentence naming whose sign-in it is", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]); // owner alice@corp
    mount(true, "admin@corp");
    const panel = await screen.findByTestId("live-approvals");
    // The admin's own sign-in captures into the ADMIN's scope and can never
    // resolve alice's row — the button would be a gesture the server refuses.
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).not.toBeInTheDocument();
    // …and "waiting on YOUR sign-in" is the false half of the same defect.
    expect(within(panel).queryByText(REAUTH_ROW.hint)).not.toBeInTheDocument();
    expect(within(panel).getByText(REAUTH_ROW.notYoursHint("alice@corp"))).toBeInTheDocument();
  });

  it("says nothing audience-dependent until /me has answered — no door on an unresolved viewer", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]);
    render(
      <OperatorProvider operator operatorResolved={false} securityOperator principal="alice@corp">
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <LiveApprovals runId="r1" />
        </ModelAccessProvider>
      </OperatorProvider>,
    );
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).not.toBeInTheDocument();
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

// M-7 (admin-member-modes-design.md §4.6, §6) — the admin monitor carries no
// personal door, even on the admin's OWN row: it reads exactly like a
// non-owner reading a member's row (the door gone, the not-yours sentence),
// plus a switch link back to it. The shared lane is untouched.
describe("LiveApprovals — the reauth row in the admin view (M-7)", () => {
  beforeEach(() => {
    listApprovalsMock.mockReset().mockResolvedValue([]);
  });

  it("gives the admin's own row the not-yours sentence and no door", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow({ requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner: "admin@corp" } })]);
    mount(true, "admin@corp", true);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).not.toBeInTheDocument();
    expect(within(panel).getByText(REAUTH_ROW.notYoursHint("admin@corp"))).toBeInTheDocument();
  });

  it("…and offers the switch link back to it, only on that own row", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow({ requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner: "admin@corp" } })]);
    mount(true, "admin@corp", true);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeInTheDocument();
  });

  it("gives a member's row no switch link — it is not the admin's own", async () => {
    listApprovalsMock.mockResolvedValue([reauthRow()]); // owner alice@corp
    mount(true, "admin@corp", true);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
    expect(within(panel).getByText(REAUTH_ROW.notYoursHint("alice@corp"))).toBeInTheDocument();
  });

  it("leaves the shared lane's door alone — it stays an admin-mode control", async () => {
    listApprovalsMock.mockResolvedValue([
      reauthRow({ requested_scope: { mechanism: "bedrock_sso", credential_source: "shared", owner: "" } }),
    ]);
    mount(true, "admin@corp", true);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
  });
});
