/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run cockpit's Azure DevOps integration (plan slice S10 round 2, F2):
// the Approvals tab must route an escalation to AdoCapabilityCard with an
// explicit decision_scope, and the Overview tab's viewerBlocked banner must
// not claim a run's own owner is blocked on an admin decision their own card
// already lets them make. Its own file rather than more cases in
// run-detail.test.tsx (915 lines, one shared api fake every other describe
// depends on) — same reason run-detail.test.tsx's own "Approvals tab" describe
// gives for keeping that one self-contained.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
  },
}));
const listApprovalsMock = vi.fn().mockResolvedValue([]);
const approveMock = vi.fn().mockResolvedValue({});
const denyMock = vi.fn().mockResolvedValue({});
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({
  recordings: { getRecording: vi.fn().mockResolvedValue(null) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), warning: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";
import { VIEWER_APPROVAL_BLOCKS_NOTE } from "../wardyn/copy";

beforeEach(() => {
  vi.clearAllMocks();
  getRunMock.mockReset();
  listApprovalsMock.mockReset();
  listApprovalsMock.mockResolvedValue([]);
  approveMock.mockReset();
  approveMock.mockResolvedValue({});
});

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "dana@acme.example",
  agent: "claude-code",
  repo: "acme/payments-api",
  task: "checkout retry",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

const ESCALATION = {
  id: "esc-1",
  run_id: "run-1",
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

function renderOwnedBy(principal: string) {
  getRunMock.mockResolvedValue(RUN);
  listApprovalsMock.mockResolvedValue([ESCALATION]);
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <OperatorProvider operator={false} securityOperator={false} principal={principal}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("RunDetailScreen — the Approvals tab's Azure DevOps capability card (F2)", () => {
  it("the run's own owner (no operator tier) can decide it, with an explicit scope", async () => {
    renderOwnedBy("dana@acme.example");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    const card = await screen.findByTestId("ado-capability-card");
    await waitFor(() => expect(card).toHaveTextContent("Push"));
    const approveBtn = screen.getByRole("button", { name: "Approve" });
    expect(approveBtn).not.toBeDisabled();
    await user.click(approveBtn);
    expect(approveMock).toHaveBeenCalledWith("esc-1", expect.any(String), { scope: "run" });
  });

  it("neg: a non-owning, non-operator viewer gets 'Not yours to decide' and no button", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([ESCALATION]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="someone-else@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    expect(await screen.findByText("Not yours to decide")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });
});

// N1 (round 2) — listApprovals("", id) returns EVERY state, not just
// PENDING, so a decided Azure DevOps row reaches this tab too. It must fall
// through to the generic decided row (with the F11 OUTCOME badge), never to
// AdoCapabilityCard, which has no "decided" rendering at all — before this
// fix it showed live Approve/Deny buttons (and, on an ended run, a false
// "nothing to allow") over a row nobody can act on any more.
describe("RunDetailScreen — decided Azure DevOps rows on the Approvals tab (N1)", () => {
  it("an APPROVED escalation falls through to the generic row with the 'Allowed for this run' badge", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([
      { ...ESCALATION, state: "APPROVED", decided_by: "dana@acme.example", decision_scope: "run" },
    ]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    await screen.findByText(/Decided by/);
    expect(screen.queryByTestId("ado-capability-card")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(screen.getByText(/Allowed for this run/)).toBeInTheDocument();
  });

  it("a DENIED escalation falls through to the generic row, no Approve/Deny buttons", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([
      { ...ESCALATION, state: "DENIED", decided_by: "dana@acme.example", decision_scope: "once" },
    ]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    await screen.findByText(/Decided by/);
    expect(screen.queryByTestId("ado-capability-card")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
  });

  it("a resolved (APPROVED) Entra-consent row falls through to the generic row, not AdoConsentCard", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([
      {
        id: "consent-1",
        run_id: "run-1",
        kind: "credential_reauth",
        requested_scope: { lane: "azure_devops", mechanism: "entra_consent", owner: "dana@acme.example", provider_id: "row_1", scopes: [] },
        state: "APPROVED",
        decided_by: "dana@acme.example",
        requested_at: new Date().toISOString(),
      },
    ]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    await screen.findByText(/Decided by/);
    expect(screen.queryByTestId("ado-consent-card")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Allow and continue" })).not.toBeInTheDocument();
  });

  it("an ended run with an APPROVED escalation never says 'nothing to allow'", async () => {
    getRunMock.mockResolvedValue({ ...RUN, state: "COMPLETED" });
    listApprovalsMock.mockResolvedValue([
      { ...ESCALATION, state: "APPROVED", decided_by: "dana@acme.example", decision_scope: "once" },
    ]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="dana@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    await screen.findByText(/Decided by/);
    expect(screen.queryByText(/nothing to allow/i)).not.toBeInTheDocument();
    expect(screen.getByText(/Allowed once/)).toBeInTheDocument();
  });
});

describe("RunDetailScreen — the Overview viewerBlocked banner (F2)", () => {
  it("does NOT claim the run's own owner is blocked — their own card lets them decide it", async () => {
    renderOwnedBy("dana@acme.example");
    await screen.findByRole("heading", { name: "checkout retry", level: 1 });
    expect(screen.queryByText(VIEWER_APPROVAL_BLOCKS_NOTE)).not.toBeInTheDocument();
  });

  it("still shows the banner for a viewer who cannot decide the escalation", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([ESCALATION]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="someone-else@acme.example">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    await screen.findByRole("heading", { name: "checkout retry", level: 1 });
    expect(await screen.findByText(VIEWER_APPROVAL_BLOCKS_NOTE)).toBeInTheDocument();
  });
});
