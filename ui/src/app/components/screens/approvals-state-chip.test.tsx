/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { APPROVAL_ESCALATION } from "../wardyn/copy/approvals";

// #3031: a card in the Pending tab does not wear a Pending chip (the tab already says so). Escalation chips stay,
// they say something the tab does not; decided rows keep Approved / Denied / Expired.

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

let rows: ApprovalRequest[] = [];
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: (state: string) => Promise.resolve(rows.filter((r) => !state || r.state === state)) },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1", agent: "claude-code", repo: "acme/widgets", task: "Fix flaky auth tests",
        confinement_class: "CC2", state: "RUNNING",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";

const base: ApprovalRequest = {
  id: "apr_1",
  run_id: "run_1",
  kind: "egress_domain",
  requested_scope: { host: "api.example.com" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
};

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ApprovalsScreen />
    </MemoryRouter>,
  );
}

describe("ApprovalsScreen — the state chip", () => {
  it("a Pending-tab card carries no Pending chip, and keeps its escalation chip", async () => {
    rows = [{ ...base, escalation_tier: 1 }];
    renderAt("/admin/approvals");
    expect(await screen.findByText(APPROVAL_ESCALATION.ESCALATED(1))).toBeInTheDocument();
    expect(screen.getByText("Reach api.example.com")).toBeInTheDocument();
    // The tab trigger reads "Pending"; nothing else on the page may.
    const pendingTexts = screen.queryAllByText("Pending", { exact: true }).filter((el) => el.getAttribute("role") !== "tab");
    expect(pendingTexts).toHaveLength(0);
  });

  it("a decided row keeps its Approved chip", async () => {
    rows = [{ ...base, state: "APPROVED", decided_by: "alice", decided_at: new Date().toISOString() }];
    renderAt("/admin/approvals?tab=decided");
    expect(await screen.findByText("Approved", { exact: true })).toBeInTheDocument();
  });
});
