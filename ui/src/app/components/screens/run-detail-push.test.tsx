/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #181 review finding 3 — the run cockpit's Approvals tab must route a
// PENDING push_content row to PushContentCard, and a DECIDED one to a short
// summary, never to the generic row's raw-scope JsonBlock (which would
// expose acts_as and, for an Azure DevOps REST push, the request-body digest
// under the wire key "commits"). Its own file, the same split
// run-detail-ado.test.tsx already uses for its own kind-specific card.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
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

const PUSH_ROW = {
  id: "push-1",
  run_id: "run-1",
  kind: "push_content",
  requested_scope: {
    repo: "github.com/acme/payments-api",
    branch: "refs/heads/feature/checkout",
    acts_as: "github_token:11111111-1111-1111-1111-111111111111",
    paths: [".github/workflows/deploy.yml"],
    paths_total: 1,
    commits: ["deadbeef".repeat(5)],
    paths_digest: "a".repeat(64),
    acts_as_kind: "github_app",
    acts_as_label: "dana@acme.example",
  },
  state: "PENDING",
  requested_at: new Date().toISOString(),
};

function renderAsAdmin() {
  getRunMock.mockResolvedValue(RUN);
  listApprovalsMock.mockResolvedValue([PUSH_ROW]);
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <Routes>
        <Route path="/runs/:id" element={<RunDetailScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("RunDetailScreen — the Approvals tab's push_content card (#181)", () => {
  it("routes a PENDING row to PushContentCard, not the generic raw-scope row, and decides with no opts", async () => {
    renderAsAdmin();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    const card = await screen.findByTestId("push-content-card");
    expect(card).toHaveTextContent("github.com/acme/payments-api");
    expect(card).toHaveTextContent("dana@acme.example");
    // Never the raw acts_as, and never a labelled "Commits" field.
    expect(card.innerHTML).not.toContain("github_token:11111111");
    expect(card.innerHTML).not.toMatch(/commit/i);

    const approveBtn = screen.getByRole("button", { name: /Approve/ });
    await user.click(approveBtn);
    // No decision_scope at all — a literal 2-argument call.
    expect(approveMock).toHaveBeenCalledWith("push-1", "approved");
  });

  it("a member sees the card with no decision control", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([PUSH_ROW]);
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

    const card = await screen.findByTestId("push-content-card");
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Deny/ })).not.toBeInTheDocument();
    expect(card).toHaveTextContent("Requires the admin or security admin role.");
  });
});

describe("RunDetailScreen — a DECIDED push_content row on the Approvals tab (review finding 3)", () => {
  it("renders a short summary (repo/branch/acts_as_label), never the raw-scope JsonBlock — no acts_as, no 'Commits' label", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([
      { ...PUSH_ROW, state: "APPROVED", decided_by: "dana@acme.example" },
    ]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    await screen.findByText(/Decided by/);
    // Not the kind-specific card (it's a decided row, not PENDING) and not
    // the generic branch's raw JSON dump either.
    expect(screen.queryByTestId("push-content-card")).not.toBeInTheDocument();
    expect(screen.queryByText("Requested scope")).not.toBeInTheDocument();
    expect(screen.getByText("github.com/acme/payments-api")).toBeInTheDocument();
    expect(screen.getByText("refs/heads/feature/checkout")).toBeInTheDocument();
    // Appears twice: the summary's "Acts as" field and "Decided by" — both
    // the run owner, both fine to say twice.
    expect(screen.getAllByText("dana@acme.example").length).toBeGreaterThan(0);
    expect(document.body.innerHTML).not.toContain("github_token:11111111");
    expect(screen.queryByText(/^Commits?$/)).not.toBeInTheDocument();
  });
});
