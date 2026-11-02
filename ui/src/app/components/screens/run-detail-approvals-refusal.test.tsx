/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3438: the run's Approvals tab turns a request's buttons off once the server refuses the viewer with
// capability_egress_host, the way the live strip does.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

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
const listApprovalsMock = vi.fn();
const approveMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: (await importOriginal<typeof import("../../lib/api/audit")>()).exitCodeFromAudit,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({ recordings: { getRecording: vi.fn().mockResolvedValue(null) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { HttpError } from "../../lib/api/core";
import { DENIED } from "../../lib/permissions-copy";
import { APPROVALS_TAB } from "../wardyn/copy/approvals";

const ME = "member@corp";
const RUN_ROW = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: ME,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "fetch a package",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};
const EGRESS: ApprovalRequest = {
  id: "egress-1",
  run_id: "run-1",
  kind: "egress_domain",
  requested_scope: { host: "registry.example.test" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
} as ApprovalRequest;

async function decideFromTab() {
  getRunMock.mockResolvedValue(RUN_ROW);
  listApprovalsMock.mockResolvedValue([EGRESS]);
  render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <OperatorProvider operator={false} securityOperator={false} principal={ME}>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  await user.click(await screen.findByRole("tab", { name: /approvals/i }));
  const region = await screen.findByRole("region", { name: APPROVALS_TAB.WAITING });
  fireEvent.click(within(region).getByRole("button", { name: /^approve$/i }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: /^approve$/i }));
  await waitFor(() => expect(approveMock).toHaveBeenCalled());
  fireEvent.click(await within(dialog).findByRole("button", { name: /cancel/i }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  return region;
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("RunDetailScreen Approvals tab — a request the server refuses the viewer (#3438)", () => {
  it("a capability_egress_host refusal turns the card's buttons off and says why", async () => {
    approveMock.mockRejectedValueOnce(new HttpError(403, "you are not granted egress host registry.example.test", "capability_egress_host"));
    const region = await decideFromTab();
    expect(within(region).getByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(within(region).getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(within(region).getByText(DENIED.APPROVE_CHIP)).toBeInTheDocument();
    expect(within(region).getByText(DENIED.APPROVE_BODY("registry.example.test"))).toBeInTheDocument();
  });

  // The run owner's own walls deny the host: the same walls_block card the Approvals page draws.
  it("a personal_walls_host refusal turns Approve off, keeps Deny, and says whose walls block the host", async () => {
    approveMock.mockRejectedValueOnce(new HttpError(403, "the owner's walls deny registry.example.test", "personal_walls_host"));
    const region = await decideFromTab();
    expect(within(region).getByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(within(region).getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(await within(region).findByTestId("approval-walls-block")).toHaveTextContent("registry.example.test");
    expect(within(region).queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });

  // A 403 with another reason (the Always scope is admin-only) must not read as the host being withheld.
  it.each([
    [409, "approval_not_pending"],
    [403, "security_admin_surface"],
  ])("a %i %s refusal leaves the buttons live", async (status, reason) => {
    approveMock.mockRejectedValueOnce(new HttpError(status, "refused", reason));
    const region = await decideFromTab();
    expect(within(region).getByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(within(region).getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(within(region).queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });
});
