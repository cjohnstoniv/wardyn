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
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { ModelAccessProvider } from "./model-access-context";
import type { AdoCardRun } from "./ado-capability-card";
import { SECURITY_ONLY_REASON } from "./copy";
import { ADO } from "../../lib/ado-entra-copy";

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

const OWNER: AdoCardRun = { created_by: "dana@acme.example", state: "RUNNING" };

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

function consentRow(over: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "consent-1",
    run_id: "r1",
    kind: "credential_reauth",
    requested_scope: {
      lane: "azure_devops",
      mechanism: "entra_consent",
      owner: "dana@acme.example",
      provider_id: "row_1",
      scopes: ["vso.code_write"],
    },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest;
}

function mount(opts: { operator?: boolean; securityOperator?: boolean; principal?: string; run?: AdoCardRun | null } = {}) {
  const { operator = false, securityOperator = false, principal = "dana@acme.example", run = OWNER } = opts;
  return render(
    <OperatorProvider operator={operator} securityOperator={securityOperator} principal={principal}>
      <MemoryRouter>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <LiveApprovals runId="r1" run={run} />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

describe("LiveApprovals — the Azure DevOps capability card, in the run cockpit", () => {
  it("renders the full card, decidable, for the run's own owner (no operator tier at all)", async () => {
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "dana@acme.example" });
    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  // F2 (round-2 fix) — round 1 hardcoded `operator` true here, on the wrong
  // assumption that every LiveApprovals mount gates on run ownership the way
  // run-detail.tsx's GET /runs/{id} does. It does not: a viewer who is
  // neither the run's owner nor a security operator sees "Not yours".
  it("a viewer who is neither the run's owner nor a security operator sees 'Not yours to decide'", async () => {
    // ticket: F2
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "someone-else@acme.example", securityOperator: false });
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByText("Not yours to decide")).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("a security operator decides it regardless of ownership", async () => {
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "someone-else@acme.example", securityOperator: true });
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("Approve sends an explicit decision_scope 'run' — never a bodyless decide", async () => {
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "dana@acme.example" });
    const card = await screen.findByTestId("ado-capability-card");
    await userEvent.click(within(card).getByRole("button", { name: "Approve" }));
    expect(approveMock).toHaveBeenCalledWith("esc-1", expect.any(String), { scope: "run" });
  });

  // F2 test gap (round 2) — the strip's own SECURITY_ONLY_REASON hint must
  // not show when the viewer CAN in fact decide this row (their own run).
  it("the strip header does NOT show SECURITY_ONLY_REASON to the run's own owner", async () => {
    // ticket: F2
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "dana@acme.example" });
    await screen.findByTestId("ado-capability-card");
    expect(screen.queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
  });

  // N4 (round 2) — record-pane.tsx and demo-runner.tsx pass run={null}
  // (they have no AgentRun in hand), but their OWN list fetch is already
  // ownership-gated server-side — a row reaching `pending` at all already
  // proves this viewer may decide it. Before this fix, run=null showed the
  // owner "Couldn't load this run — try again." and the SECURITY_ONLY_REASON
  // hint, over a row their own list scoping already proved was theirs.
  it("run=null still renders decidable for a plain viewer — no run-fetch error, no admin-only header", async () => {
    // ticket: N4
    listApprovalsMock.mockResolvedValue([escalationRow()]);
    mount({ principal: "dana@acme.example", run: null });
    const card = await screen.findByTestId("ado-capability-card");
    expect(within(card).queryByText(/Couldn't load this run/)).not.toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(screen.queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
  });

  it("an ordinary tool_call row (no Azure DevOps lane) still renders the strip's plain one-liner", async () => {
    listApprovalsMock.mockResolvedValue([
      { id: "t1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "bash", cmd: "rm -rf build/" }, state: "PENDING", requested_at: new Date().toISOString() } as ApprovalRequest,
    ]);
    mount({ principal: "dana@acme.example" });
    await screen.findByTestId("live-approval-row");
    expect(screen.queryByTestId("ado-capability-card")).not.toBeInTheDocument();
  });

  // F3 (round-2 fix) — the consent state's heading must not claim AWS, and
  // the owner gets the door.
  describe("the Entra-consent row", () => {
    it("the strip heading names Azure DevOps, never AWS, when every pending row is a consent request", async () => {
      // ticket: F3
      listApprovalsMock.mockResolvedValue([consentRow()]);
      mount({ principal: "dana@acme.example" });
      await screen.findByTestId("ado-consent-card");
      expect(screen.getByText(/Azure DevOps sign-in needed/)).toBeInTheDocument();
      expect(screen.queryByText(/AWS sign-in needed/)).not.toBeInTheDocument();
    });

    it("the owner gets the consent chip and the door, to the Settings card's anchor (#458)", async () => {
      // ticket: F3
      listApprovalsMock.mockResolvedValue([consentRow()]);
      mount({ principal: "dana@acme.example" });
      const card = await screen.findByTestId("ado-consent-card");
      expect(within(card).getByText("Needs your Microsoft consent")).toBeInTheDocument();
      const cta = within(card).getByRole("link", { name: ADO.REQ_CONSENT_CTA });
      expect(cta).toBeInTheDocument();
      expect(cta).toHaveAttribute("href", "/settings#azure-devops");
    });
  });
});
