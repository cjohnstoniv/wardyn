/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// W25-1: SummaryHeader's "Interactive — attachable" chip must claim
// attachability under the SAME predicate AttachTerminal itself gates on
// (attach-terminal.tsx: `if (!operator) { ...requires the operator role }`)
// — otherwise a member sees the chip promise attachability and then gets a
// red "requires the operator role" error the instant they open the terminal
// below it (OverviewTab renders <AttachTerminal> whenever `attachable`).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { SummaryHeader } from "./run-detail-summary-header";
import { OperatorProvider } from "../wardyn/operator-context";
import type { AgentRun } from "../../lib/types";

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
    render(
      <OperatorProvider operator={false}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive")).toBeInTheDocument();
    expect(screen.queryByText("Interactive — attachable")).toBeNull();
  });

  it("shows 'Interactive — attachable' for an operator on a RUNNING interactive run", () => {
    render(
      <OperatorProvider operator={true}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive — attachable")).toBeInTheDocument();
  });
});
