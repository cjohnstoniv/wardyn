/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #160 — TitleGroup's second chip row: one counted chip per reason a group's
// runs are waiting, and a pinned "Checking…" before the approvals fetch
// resolves. Its own file, extracted alongside title-group.tsx so
// runs.test.tsx stays under the size gate.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun, ApprovalRequest, RunState } from "../../../lib/types";
import { TitleGroup } from "./title-group";
import { approvalSignals, type RunSignals } from "./board-groups";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "e2e fixture",
  title: "Nightly audit",
  confinement_class: "CC2",
  state: "RUNNING" as RunState,
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  ...over,
});

const approval = (over: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id: "a1",
  run_id: "run-1",
  kind: "tool_call",
  requested_scope: {},
  state: "PENDING",
  requested_at: new Date().toISOString(),
  ...over,
});

function renderGroup(runs: AgentRun[], signals: RunSignals = new Map(), signalsResolved = true) {
  return render(
    <MemoryRouter>
      <TitleGroup
        title="Nightly audit"
        runs={runs}
        signals={signals}
        signalsResolved={signalsResolved}
        open
        onToggle={vi.fn()}
        onOpen={vi.fn()}
        onKill={vi.fn()}
      />
    </MemoryRouter>,
  );
}

function waitRow(): HTMLElement {
  return screen.getByLabelText("What this group is waiting on");
}

describe("TitleGroup — the wait row (#160)", () => {
  it("five runs — two held, one awaiting sign-in, one image-pull failure, one clean — render exactly three counted chips and no fourth", () => {
    const signals = approvalSignals([
      approval({ id: "a1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" } }),
      approval({ id: "a2", run_id: "r2", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" } }),
      approval({ id: "a3", run_id: "r3", kind: "credential_reauth", requested_scope: {} }),
    ]);
    const runs = [
      run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r2", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r3", state: "RUNNING" }),
      run({ id: "r4", state: "STARTING" }),
      run({ id: "r5", state: "RUNNING" }), // clean — counted nowhere
    ];
    renderGroup(runs, signals, true);

    const row = waitRow();
    expect(within(row).getByText("2 awaiting confirmation")).toBeInTheDocument();
    expect(within(row).getByText("1 awaiting AWS sign-in")).toBeInTheDocument();
    expect(within(row).getByText("1 waiting to start")).toBeInTheDocument();
    expect(row.children).toHaveLength(3);
    expect(within(row).queryByText("Nothing waiting")).not.toBeInTheDocument();
    expect(within(row).queryByText("Checking…")).not.toBeInTheDocument();
  });

  it("shows Checking… before the approvals promise resolves — nothing derived from it paints early", () => {
    const runs = [run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" })];
    renderGroup(runs, new Map(), false);
    const row = waitRow();
    expect(within(row).getByText("Checking…")).toBeInTheDocument();
    expect(within(row).queryByText(/awaiting confirmation/)).not.toBeInTheDocument();
  });

  it("Checking… still lets the STARTING chip through — that count is a run.state fact, not an approvals one", () => {
    const runs = [run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" }), run({ id: "r2", state: "STARTING" })];
    renderGroup(runs, new Map(), false);
    const row = waitRow();
    expect(within(row).getByText("Checking…")).toBeInTheDocument();
    expect(within(row).getByText("1 waiting to start")).toBeInTheDocument();
  });

  it("'Nothing waiting' is uncounted, and only appears when the whole (non-terminal) group is clean", () => {
    const runs = [run({ id: "r1", state: "RUNNING" }), run({ id: "r2", state: "PENDING" })];
    renderGroup(runs, new Map(), true);
    const row = waitRow();
    expect(within(row).getByText("Nothing waiting")).toBeInTheDocument();
    expect(row.children).toHaveLength(1);
  });

  it("is suppressed entirely once every run in the group is terminal", () => {
    const runs = [run({ id: "r1", state: "COMPLETED" }), run({ id: "r2", state: "FAILED" })];
    renderGroup(runs, new Map(), true);
    expect(screen.queryByLabelText("What this group is waiting on")).not.toBeInTheDocument();
  });

  // #509 — a PENDING tool_call stays counted as held at any age; only the
  // server's own state (never client elapsed time) can move it off the
  // counted "awaiting confirmation" chip.
  it("a tool_call held for hours still counts in the live 'awaiting confirmation' chip", () => {
    const old = new Date(Date.now() - 2 * 60 * 60_000).toISOString();
    const signals = approvalSignals([
      approval({ id: "a1", run_id: "r1", kind: "tool_call", requested_scope: { tool: "Bash", cmd: "ls" } }),
      approval({
        id: "a2",
        run_id: "r2",
        kind: "tool_call",
        requested_scope: { tool: "Bash", cmd: "ls" },
        requested_at: old,
      }),
    ]);
    const runs = [
      run({ id: "r1", state: "WAITING_FOR_CONFIRMATION" }),
      run({ id: "r2", state: "WAITING_FOR_CONFIRMATION" }),
    ];
    renderGroup(runs, signals, true);
    const row = waitRow();
    expect(within(row).getByText("2 awaiting confirmation")).toBeInTheDocument();
    expect(within(row).queryByText(/was held/)).not.toBeInTheDocument();
  });
});
