/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// W25-1: SummaryHeader's "Interactive — attachable" chip must claim
// attachability under the SAME predicate AttachTerminal itself gates on
// (attach-terminal.tsx: `if (!operator) { ...requires the admin role }`)
// — otherwise a member sees the chip promise attachability and then gets a
// red "requires the admin role" error the instant they open the terminal
// below it (OverviewTab renders <AttachTerminal> whenever `attachable`).
import type { ReactElement } from "react";
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SummaryHeader } from "./run-detail-summary-header";
import { OperatorProvider } from "../wardyn/operator-context";
import type { AgentRun } from "../../lib/types";

// SummaryHeader now renders a "Runs" breadcrumb <Link> (react-router-dom),
// which throws outside a Router context — wrap every render the same way
// run-detail-ssh.test.tsx does for its own router-dependent screen.
function renderHeader(ui: ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

const runningInteractive: AgentRun = {
  id: "run-1",
  created_at: "now",
  updated_at: "now",
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  interactive: true,
};

describe("SummaryHeader — attachable chip predicate (W25-1)", () => {
  it("shows the plain 'Interactive' chip (no attachable claim) for a member", () => {
    renderHeader(
      <OperatorProvider operator={false}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive")).toBeInTheDocument();
    expect(screen.queryByText("Interactive — attachable")).toBeNull();
  });

  it("shows 'Interactive — attachable' for an operator on a RUNNING interactive run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive — attachable")).toBeInTheDocument();
  });
});

// Command-bar reshape (design board seg2a): the fat identity card became a
// 52px row. These pin the shape that survived the squeeze — task as the
// page's h1, terminal-disabled Kill, and the pending-approvals chip.
describe("SummaryHeader — command bar", () => {
  const taskRun: AgentRun = { ...runningInteractive, task: "audit the egress proxy for missing hosts" };

  it("renders run.task as the page's h1", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(
      screen.getByRole("heading", { name: "audit the egress proxy for missing hosts", level: 1 }),
    ).toBeInTheDocument();
  });

  it("disables Kill on a terminal run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={{ ...taskRun, state: "COMPLETED" }} terminal={true} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: /kill/i })).toBeDisabled();
  });

  it("keeps Kill enabled on a live run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: /kill/i })).toBeEnabled();
  });

  it("shows the pending-approval count when non-zero", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} pendingApprovalCount={2} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText(/2 waiting/i)).toBeInTheDocument();
  });

  it("renders no pending-approval chip when the count is zero", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} pendingApprovalCount={0} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.queryByText(/waiting/i)).toBeNull();
  });
});
