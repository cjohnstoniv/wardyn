/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// fix: "Run not found" only ever listed archived / deleted / stale-link as
// possible reasons — never "you don't have access" — even though the Copy
// link feature two lines above (copyLink) routinely produces exactly that
// case: a member pastes a coworker's copied run link and hits this state
// because they lack access, not because the run is gone. This pins that the
// description now names that reason too (without disambiguating which case
// applies — preserves the anti-enumeration property).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    // The cockpit's evidence widgets poll these. They reject here so the rail
    // renders its degraded states — this suite is about the cockpit's own
    // layout decisions, not the widgets (widgets.test.tsx owns those).
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
  },
}));
const listApprovalsMock = vi.fn().mockResolvedValue([]);
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({
  // Resolves, rather than a bare vi.fn() returning undefined: a FINISHED run on
  // Overview now fetches its cast on mount (the hero pane replays in place), so
  // a mock that is not thenable takes the whole screen down.
  recordings: { getRecording: vi.fn().mockResolvedValue(null) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));

import { RunDetailScreen } from "./run-detail";
import { RUN_COCKPIT } from "../wardyn/copy";

beforeEach(() => {
  getRunMock.mockReset();
  listApprovalsMock.mockReset();
  listApprovalsMock.mockResolvedValue([]);
});

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

function renderRun(run: Record<string, unknown>) {
  getRunMock.mockResolvedValue(run);
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <Routes>
        <Route path="/runs/:id" element={<RunDetailScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("RunDetailScreen — 'Run not found' names lack-of-access as a real reason", () => {
  it("mentions not having access alongside archived/deleted/stale-link", async () => {
    getRunMock.mockResolvedValue(undefined);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Run not found")).toBeInTheDocument();
    expect(screen.getByText(/don't have access/i)).toBeInTheDocument();
  });
});

// The cockpit's own state machine — which pane the hero shows. The four
// Terminal situations of design board 2d split across two files: states 1 and 2
// (driving / held by another client) live in attach-terminal.tsx and are pinned
// by attach-terminal.test.tsx, because only the socket knows its attach mode.
// These are the two the PARENT decides, from facts about the run.
describe("RunDetailScreen — the hero pane per run situation", () => {
  it("an autonomous run gets the Output pane, not a terminal it cannot open", async () => {
    renderRun({ ...RUN, interactive: false, state: "RUNNING" });
    // RUN_MODE.autonomous.blurb — there is no PTY to type into.
    expect(await screen.findByText(/Runs unattended/i)).toBeInTheDocument();
    expect(screen.getByText("Output")).toBeInTheDocument();
  });

  it("a finished run replays in place rather than leaving the biggest pane dead", async () => {
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    // Match the pane's own chip, not the word "Recording" — that also names the
    // fourth TAB, and a query that matched both would pass even if the hero
    // pane were empty, which is the exact thing this test exists to catch.
    const pane = await screen.findByTestId("run-terminal-pane");
    expect(pane).toHaveTextContent(RUN_COCKPIT.finishedReplay);
  });

  it("keeps run.task as the page's h1 — for an autonomous run it is the only statement of what it is doing", async () => {
    renderRun({ ...RUN });
    expect(
      await screen.findByRole("heading", { name: "audit the egress proxy", level: 1 }),
    ).toBeInTheDocument();
  });
});

// THE POINT OF THE REDESIGN, and therefore the assertion most worth pinning: a
// held egress request renders in the TERMINAL'S OWN COLUMN, under the output
// that caused it — not in a sidebar, not a toast. Asserting it exists somewhere
// on the page would pass for a layout that put it back in the rail, so this
// asserts CONTAINMENT.
describe("RunDetailScreen — the held approval renders inside the terminal pane", () => {
  it("puts the live-approvals strip inside the terminal pane, not the evidence rail", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com", mode: "wait_for_review" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });

    const strip = await screen.findByTestId("live-approvals");
    const pane = screen.getByTestId("run-terminal-pane");
    expect(pane).toContainElement(strip);
  });

  it("states the hold in the command bar too, so it is visible without scrolling", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com", mode: "wait_for_review" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });
    // waitingHeld, not waiting: a parked sandbox is a different fact from a
    // merely queued approval, and isHeld is shared with the strip below so the
    // two can never disagree.
    expect(await screen.findByText(/sandbox held/i)).toBeInTheDocument();
  });
});
