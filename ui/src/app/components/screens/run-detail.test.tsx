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
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
const listAuditMock = vi.fn().mockResolvedValue([]);
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
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
  listAuditMock.mockReset();
  listAuditMock.mockResolvedValue([]);
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

// hasWorkspace must cover workspace_id, not just workspace_ids: a record/
// verify step run carries the former only (its workspace_ids is always
// empty — see runHasWorkspace's doc in lib/types/runs.ts), and the server's
// rule-5 tie-break accepts `always` there. Reading workspace_ids alone showed
// Always disabled with the false reason "this run isn't attached to one".
describe("RunDetailScreen — hasWorkspace covers workspace_id, not just workspace_ids", () => {
  it("a record/verify-shaped run (workspace_id set, workspace_ids empty) offers Always", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING", workspace_ids: [], workspace_id: "ws-1" });

    const strip = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(strip).getAllByRole("button", { name: /more options/i })[0]);

    const always = await screen.findByRole("button", { name: /^Always/ });
    expect(always).not.toBeDisabled();
    expect(screen.queryByText(/isn't attached to one/i)).not.toBeInTheDocument();
  });

  it("an ordinary run with neither field set still shows Always disabled, with why", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });

    const strip = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(strip).getAllByRole("button", { name: /more options/i })[0]);

    const always = await screen.findByRole("button", { name: /^Always/ });
    expect(always).toBeDisabled();
    expect(screen.getByText(/isn't attached to one/i)).toBeInTheDocument();
  });
});

// W17-S1-3: the run-detail Audit tab's own fetch is capped at LIST_LIMIT
// (server: auditPerRunDefaultLimit) and returned oldest-first — a chatty run's
// later events can silently fall off the end. A capped page must say so; a
// page under the cap must not.
// 15s suite default, not vitest's 5s: the capped case renders a 1000-row audit
// list and then drives a userEvent click through it — under a second alone, but
// deterministically over the 5s ceiling when all 79 files run in parallel.
// Same rationale (and the same 1000-row feed) as audit.test.tsx's suite timeout.
describe("RunDetailScreen — Audit tab truncation cue", { timeout: 15_000 }, () => {
  function auditEvent(i: number) {
    return {
      id: `e${i}`,
      time: new Date().toISOString(),
      actor_type: "agent",
      actor: "spiffe://wardyn/agent",
      action: "kernel.process.exec",
      outcome: "success",
      run_id: "run-1",
    };
  }

  it("shows a truncation cue when the per-run window hits the 1000 cap", async () => {
    listAuditMock.mockImplementation((_id: string, action?: string) =>
      Promise.resolve(action ? [] : Array.from({ length: 1000 }, (_, i) => auditEvent(i))),
    );
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByText(/truncated, oldest-first/i)).toBeInTheDocument();
  });

  it("shows no truncation cue under the cap", async () => {
    listAuditMock.mockImplementation((_id: string, action?: string) =>
      Promise.resolve(action ? [] : [auditEvent(1)]),
    );
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    await screen.findByText(/1 event for this run/i);
    expect(screen.queryByText(/truncated, oldest-first/i)).not.toBeInTheDocument();
  });
});

// W25-W25.2-3: /audit is run-scoped for a member (empty 200 without ?run_id=),
// so the Audit tab's "open full Audit" link must carry the run — a bare /audit
// drops a member on a feed that can never fill.
describe("RunDetailScreen — open full Audit link", () => {
  it("carries the run id into /audit", async () => {
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByRole("link", { name: /open full audit/i })).toHaveAttribute(
      "href",
      "/audit?run_id=run-1",
    );
  });
});
