/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { APPROVAL_ESCALATION } from "../wardyn/copy/approvals";

// notify-e4 (packet M6 S2): a PENDING egress card shows "Escalated · level n" when
// the server says a tier past the first is in force, and "Escalates in …" when a
// later tier is scheduled. Either, both or neither; never on a decided row.

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

function renderScreen() {
  return render(
    <MemoryRouter>
      <ApprovalsScreen />
    </MemoryRouter>,
  );
}

describe("ApprovalsScreen — escalation chips", () => {
  it("shows both chips with the M6 strings when a tier is in force and the next is due", async () => {
    rows = [{ ...base, escalation_tier: 1, sla_due_at: new Date(Date.now() + 40 * 60_000 + 5_000).toISOString() }];
    renderScreen();
    const escalated = await screen.findByText(APPROVAL_ESCALATION.ESCALATED(1));
    expect(escalated).toHaveAttribute("title", APPROVAL_ESCALATION.ESCALATED_TITLE);
    expect(screen.getByText("Escalates in 40m")).toBeInTheDocument();
  });

  it("shows only the countdown chip before any escalation", async () => {
    rows = [{ ...base, sla_due_at: new Date(Date.now() + 3 * 3_600_000 + 5_000).toISOString() }];
    renderScreen();
    expect(await screen.findByText("Escalates in 3h")).toBeInTheDocument();
    expect(screen.queryByText(/^Escalated/)).not.toBeInTheDocument();
  });

  it("shows only the escalated chip once no tier is left", async () => {
    rows = [{ ...base, escalation_tier: 2 }];
    renderScreen();
    expect(await screen.findByText("Escalated · level 2")).toBeInTheDocument();
    expect(screen.queryByText(/^Escalates/)).not.toBeInTheDocument();
  });

  it("shows neither chip when the server sent neither field", async () => {
    rows = [base];
    renderScreen();
    await screen.findByText("Reach api.example.com");
    expect(screen.queryByText(/^Escalat/)).not.toBeInTheDocument();
  });
});
