/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

// fix: the board's "Kill run" action used to fire api.killRun immediately
// from the dropdown — no confirmation — unlike the identical action on Run
// Detail, which is AlertDialog-gated. These tests pin that the board now asks
// first.

const listRunsMock = vi.fn();
const killRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    listRuns: (...a: unknown[]) => listRunsMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
  },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

// new-run-dialog.tsx is a sibling-owned, API-heavy component (workspaces,
// composer backends, setup status…) — stubbed here so these tests exercise
// ONLY runs.tsx's own mount/open wiring, not that component's internals.
vi.mock("./new-run/new-run-dialog", () => ({
  NewRunDialog: ({ open }: { open: boolean }) =>
    open ? <div role="dialog" aria-label="new run dialog stub" /> : null,
}));

import { RunsScreen } from "./runs";
import { RoleProvider, type Role } from "../wardyn/operator-context";
import { baseStatus } from "../../lib/test-fixtures";
import { DEMOS } from "./demos/demo-catalog";

const run: AgentRun = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "Fix flaky auth tests",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
};

function renderScreen(role?: Role) {
  const tree = <RunsScreen />;
  return render(
    <MemoryRouter>{role ? <RoleProvider role={role}>{tree}</RoleProvider> : tree}</MemoryRouter>,
  );
}

beforeEach(() => {
  listRunsMock.mockReset();
  killRunMock.mockReset();
  getSetupStatusMock.mockReset();
  listRunsMock.mockResolvedValue([run]);
  killRunMock.mockResolvedValue(undefined);
  // Non-blocking default: both barrier tiers available, nothing to re-check.
  getSetupStatusMock.mockResolvedValue(baseStatus({ ready: true }));
});

// Stage-1: the deleted 12-step setup funnel's replacement lives entirely on
// this screen now — the first-run checklist, the demo grid, and the one hard
// blocker in the product (no sandbox barrier).
describe("RunsScreen — first-run empty state", () => {
  it("shows the first-run experience with a real barrier readout and the demo grid when there are no runs", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen();

    expect(await screen.findByText("No runs yet")).toBeInTheDocument();
    expect(
      screen.getByText(
        "A run is a workload in a sealed box. You watch it, approve what it reaches for, and keep the recording.",
      ),
    ).toBeInTheDocument();
    // Derived from the mocked confinement_classes (CC1 + CC2) — never a
    // hardcoded string.
    expect(await screen.findByText("Fence, Wall available on this host.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /new run/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /try it without a repo/i })).toHaveAttribute("href", "/demos");
    // The guided funnel is one unobtrusive link, not a competing button.
    expect(screen.getByRole("link", { name: /guided tour/i })).toHaveAttribute("href", "/setup");
    for (const d of DEMOS) {
      const card = within(screen.getByTestId(`runs-empty-demo-${d.id}`));
      expect(card.getByText(d.title)).toBeInTheDocument();
      expect(card.getByText(d.teaches)).toBeInTheDocument();
      if (d.needsModel) {
        // Muted with a Connect link, not a "Run it" button, since this mock's
        // setup status carries no model provider.
        expect(card.queryByRole("link", { name: "Run it" })).toBeNull();
        expect(card.getByText(/needs a model provider/i)).toBeInTheDocument();
      } else {
        expect(card.getByRole("link", { name: "Run it" })).toHaveAttribute("href", "/demos");
      }
    }
  });

  it("renders the non-dismissible no-barrier banner above everything when confinement_classes is empty, even with runs present", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ ready: false, runner: { driver: "docker", confinement_classes: [] } }),
    );
    renderScreen();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "No sandbox barrier on this host. Runs cannot start.",
    );
    expect(screen.getByText("sudo wardyn setup fence")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeInTheDocument();
    // The board itself still renders underneath — the banner sits above it,
    // it doesn't replace the screen.
    expect(await screen.findByText(run.task)).toBeInTheDocument();
  });

  it("never shows the no-barrier banner for a merely-unreachable daemon", async () => {
    getSetupStatusMock.mockResolvedValue({
      ...baseStatus({ runner: { driver: "none", confinement_classes: [] } }),
      unreachable: true,
    });
    renderScreen();

    await screen.findByText(run.task);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText("No sandbox barrier on this host. Runs cannot start.")).toBeNull();
  });
});

describe("RunsScreen board — Kill run confirms before killing", () => {
  it("does not call api.killRun until the confirm dialog is accepted", async () => {
    renderScreen();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /run actions/i }));
    await user.click(await screen.findByRole("menuitem", { name: /kill run/i }));

    // The same confirm copy Run Detail uses, naming the run id.
    expect(await screen.findByText(/kill run-1\?/i)).toBeInTheDocument();
    expect(killRunMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^kill run$/i }));
    await waitFor(() => expect(killRunMock).toHaveBeenCalledWith("run-1"));
  });

  it("cancelling the confirm dialog never kills the run", async () => {
    renderScreen();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /run actions/i }));
    await user.click(await screen.findByRole("menuitem", { name: /kill run/i }));
    await screen.findByText(/kill run-1\?/i);

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(killRunMock).not.toHaveBeenCalled();
  });

  it("Enter on the kebab opens the menu, not the run — the card's own row handler must not steal it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { Route, Routes } = await import("react-router-dom");
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <Routes>
          <Route path="/runs" element={<RunsScreen />} />
          <Route path="/runs/:id" element={<div>detail for {"{id}"}</div>} />
        </Routes>
      </MemoryRouter>,
    );
    const kebab = await screen.findByRole("button", { name: /run actions/i });
    kebab.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("menu")).toBeInTheDocument();
    expect(screen.queryByText("detail for {id}")).not.toBeInTheDocument();
  });
});

// #10/D14: workspace-detail's "Start a run" CTA navigates here with route
// state instead of a stale pre-seed promise — this dialog already opens on
// the workspace-first picker, so opening it on arrival is the whole fix.
describe("RunsScreen — opens the New Run dialog from route state (workspace-detail's Start-a-run CTA)", () => {
  it("mounts and opens the dialog when arriving with location.state.openNewRun", async () => {
    render(
      <MemoryRouter initialEntries={[{ pathname: "/runs", state: { openNewRun: true } }]}>
        <RunsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("dialog", { name: "new run dialog stub" })).toBeInTheDocument();
  });

  it("does not open the dialog on an ordinary arrival with no route state", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i }); // the board settled
    expect(screen.queryByRole("dialog", { name: "new run dialog stub" })).not.toBeInTheDocument();
  });

  it("clears the route state after opening, so a back-navigation or refresh can't reopen it", async () => {
    function LocationProbe() {
      const location = useLocation();
      const s = location.state as { openNewRun?: boolean } | null;
      return <span data-testid="probe">{s?.openNewRun ? "OPEN" : "CLOSED"}</span>;
    }
    render(
      <MemoryRouter initialEntries={[{ pathname: "/runs", state: { openNewRun: true } }]}>
        <RunsScreen />
        <LocationProbe />
      </MemoryRouter>,
    );
    await screen.findByRole("dialog", { name: "new run dialog stub" });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("CLOSED"));
  });
});

// B3 (prompt-v2 point 2): the list is already server-scoped to the member's
// own runs (handleListRuns's creator-pager branch) — this only pins the copy
// says so plainly, and that admin copy is unchanged.
describe("RunsScreen — member vs admin count line", () => {
  it("a member sees \"Your runs · N\"", async () => {
    renderScreen("member");
    expect(await screen.findByText("Your runs · 1")).toBeInTheDocument();
  });

  it("an admin (and the fail-open default) keeps the unscoped description", async () => {
    renderScreen("admin");
    expect(await screen.findByText(/Every run, live/)).toBeInTheDocument();
    expect(screen.queryByText(/Your runs ·/)).not.toBeInTheDocument();
  });
});

// fix: the board card and the table row used to be role="button" tabIndex={0}
// containers directly nesting the real Attach/Review/kebab <button>s inside
// them — an invalid, double-nested interactive-widget structure (a11y
// blocker). Dropped the role/tabIndex; mouse click-to-open stays via plain
// onClick, keyboard/AT users reach the same action via the existing "Open
// detail" menu item.
describe("RunsScreen — row/card container is not itself a redundant role=button widget (a11y)", () => {
  it("board density: the card is not exposed as its own button widget nesting the real action buttons", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    // Before the fix, role="button" on the card computed its accessible name
    // from its text content — including the task text — so this query would
    // have matched the outer card div itself.
    expect(screen.queryByRole("button", { name: /fix flaky auth tests/i })).not.toBeInTheDocument();
  });

  it("table density: same — the row is not a nested role=button widget either", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /^table$/i }));
    await screen.findByRole("button", { name: /run actions/i });
    expect(screen.queryByRole("button", { name: /fix flaky auth tests/i })).not.toBeInTheDocument();
  });

  it("clicking the card body (not a button) still opens the run — mouse convenience is preserved", async () => {
    const user = userEvent.setup();
    const { Route, Routes } = await import("react-router-dom");
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <Routes>
          <Route path="/runs" element={<RunsScreen />} />
          <Route path="/runs/:id" element={<div>detail for {"{id}"}</div>} />
        </Routes>
      </MemoryRouter>,
    );
    await user.click(await screen.findByText("Fix flaky auth tests"));
    expect(await screen.findByText("detail for {id}")).toBeInTheDocument();
  });
});

// fix: the loading skeleton used to always render the Board card-grid shape,
// even in Table density — flashing the wrong skeleton on every manual
// Refresh / re-navigation while Table density was active.
describe("RunsScreen — loading skeleton matches the active density", () => {
  it("shows the Board card-grid skeleton by default, and the Table skeleton once Table density is picked while still loading", async () => {
    let resolveList!: (v: AgentRun[]) => void;
    listRunsMock.mockImplementation(
      () => new Promise<AgentRun[]>((res) => { resolveList = res; }),
    );
    const user = userEvent.setup();
    renderScreen();

    // Board density (default), still loading: 6 BoardSkeleton mini-cards.
    const boardCard = ".rounded-xl.border.border-border.bg-card.p-4";
    expect(document.querySelectorAll(boardCard).length).toBe(6);

    await user.click(screen.getByRole("button", { name: /^table$/i }));

    // Still loading (listRuns never resolved) but density is now Table — the
    // Board shape must be gone, replaced by TableSkeleton's row structure.
    expect(document.querySelectorAll(boardCard).length).toBe(0);
    expect(document.querySelector(".divide-y.divide-border")).toBeInTheDocument();

    resolveList([]);
    await waitFor(() => expect(screen.queryByRole("button", { name: /run actions/i })).not.toBeInTheDocument());
  });
});

// fix: Done section's global "Show fewer"/"Show all" toggle used to be gated
// on `shownCount >= done.length` — a count coincidence true even when nothing
// was ever expanded, whenever every group happened to fit within
// GROUP_PREVIEW. That rendered a dead "Show fewer" button wired to a
// toggleAll() no-op.
describe("RunsScreen board — Done section's global toggle is never a dead no-op control", () => {
  it("renders no global toggle when nothing is hidden and nothing is expanded (two small Done groups)", async () => {
    const doneRuns: AgentRun[] = [
      { ...run, id: "run-c1", state: "COMPLETED" },
      { ...run, id: "run-c2", state: "COMPLETED" },
      { ...run, id: "run-s1", state: "STOPPED" },
      { ...run, id: "run-s2", state: "STOPPED" },
    ];
    listRunsMock.mockResolvedValue(doneRuns);
    renderScreen();
    await screen.findByText("Done");
    expect(screen.queryByRole("button", { name: /^show fewer$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^show all$/i })).not.toBeInTheDocument();
  });
});

// fix: Runs rendered its "Live" auto-refresh indicator as plain muted text +
// a raw CircleDot icon, while Audit renders the same concept as the shared
// Chip pill primitive — two visual treatments for one concept. Runs now uses
// the same Chip.
describe("RunsScreen — Live indicator uses the shared Chip primitive (matches Audit)", () => {
  it("renders the Live copy inside the Chip pill (title carries the polling reason, like Audit's)", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    const chip = screen.getByTitle("Polling for new runs");
    expect(chip).toHaveTextContent("Live · refreshes every 3s");
  });
});
