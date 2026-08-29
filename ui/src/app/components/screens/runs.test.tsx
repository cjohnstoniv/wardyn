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
const listApprovalsMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a) },
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
  listApprovalsMock.mockReset();
  getSetupStatusMock.mockReset();
  listRunsMock.mockResolvedValue([run]);
  // Default: nothing parked on anything.
  listApprovalsMock.mockResolvedValue([]);
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
    // /demos is gone — Getting Started IS the demos surface, so both the
    // headline link and every card point at a funnel step.
    expect(screen.getByRole("link", { name: /try it without a repo/i })).toHaveAttribute(
      "href",
      "/setup?step=sealed-box",
    );
    // The guided funnel is one unobtrusive link, not a competing button.
    expect(screen.getByRole("link", { name: /guided tour/i })).toHaveAttribute("href", "/setup");
    // The grid is React.lazy'd (runs-first-run-demos.tsx) so the demo catalog's
    // prose stays out of the eager /runs entry chunk — so it arrives a tick
    // after the hero, not with it. Await the first card, then the loop is sync.
    await screen.findByTestId(`runs-empty-demo-${DEMOS[0].id}`);
    for (const d of DEMOS) {
      const card = within(screen.getByTestId(`runs-empty-demo-${d.id}`));
      expect(card.getByText(d.title)).toBeInTheDocument();
      expect(card.getByText(d.teaches)).toBeInTheDocument();
      if (d.needsModel) {
        // Muted with a Connect link, not a "Run it" button, since this mock's
        // setup status carries no model provider.
        expect(card.queryByRole("link", { name: "Run it" })).toBeNull();
        expect(card.getByText(/needs a model provider/i)).toBeInTheDocument();
      } else if (d.needsSecret) {
        // Its mirror: no secret stored in this mock's status, so the card names
        // the missing one instead of linking to a step that isn't in the walk.
        expect(card.queryByRole("link", { name: "Run it" })).toBeNull();
        expect(card.getByText(/needs the/i)).toBeInTheDocument();
        expect(card.getByText(d.needsSecret)).toBeInTheDocument();
      } else {
        // Per-card deep link: THIS demo's step, not a catalog page to hunt in.
        expect(card.getByRole("link", { name: "Run it" })).toHaveAttribute(
          "href",
          `/setup?step=${d.id}`,
        );
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

  // W2-S1-4: /setup/status is a full ListRuns plus a shell-out host sweep. It
  // is polled only while there is actually something to watch for — the
  // no-barrier blocker clearing, or an unreachable daemon coming back — never
  // forever on the landing screen of every open tab.
  it("stops polling /setup/status on a healthy host, keeps polling while the no-barrier blocker is up", async () => {
    vi.useFakeTimers();
    try {
      renderScreen();
      await vi.advanceTimersByTimeAsync(20_000);
      expect(getSetupStatusMock).toHaveBeenCalledTimes(1);

      getSetupStatusMock.mockResolvedValue(
        baseStatus({ ready: false, runner: { driver: "docker", confinement_classes: [] } }),
      );
      getSetupStatusMock.mockClear();
      renderScreen();
      await vi.advanceTimersByTimeAsync(20_000);
      expect(getSetupStatusMock.mock.calls.length).toBeGreaterThan(1);
    } finally {
      vi.useRealTimers();
    }
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
// state. That used to OPEN a dialog on this screen; New run is its own page
// now, so the same intent is a redirect to it — and the redirect replaces the
// history entry, so Back goes where the operator came from instead of bouncing
// through this screen and redirecting again.
describe("RunsScreen — Start-a-run route state redirects to the New run page", () => {
  function LocationProbe() {
    const location = useLocation();
    return <span data-testid="path">{location.pathname}</span>;
  }

  it("redirects to /runs/new when arriving with location.state.openNewRun", async () => {
    render(
      <MemoryRouter initialEntries={[{ pathname: "/runs", state: { openNewRun: true } }]}>
        <RunsScreen />
        <LocationProbe />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/runs/new"));
  });

  it("stays put on an ordinary arrival with no route state", async () => {
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <RunsScreen />
        <LocationProbe />
      </MemoryRouter>,
    );
    await screen.findByRole("button", { name: /run actions/i }); // the board settled
    expect(screen.getByTestId("path")).toHaveTextContent("/runs");
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

    // Board density (default), still loading: 6 BoardSkeleton mini-cards. The
    // skeleton matches the card's own shape (§9) — two rows in a p-3 block.
    const boardCard = ".rounded-xl.border.border-border.bg-card.p-3";
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

// fix: the board's collapse toggle used to be gated on `shownCount >=
// done.length` — a count coincidence true even when nothing had ever been
// expanded, whenever every group happened to fit within GROUP_PREVIEW. That
// rendered a dead "Show fewer" button wired to a no-op. The section it lived on
// (Done, grouped by outcome) is gone — the board groups by title now — but the
// invariant is the same and rides on the same GROUP_PREVIEW: never render a
// control that has nothing to do.
describe("RunsScreen board — a group's collapse toggle is never a dead control", () => {
  const inGroup = (id: string, over: Partial<AgentRun> = {}): AgentRun => ({
    ...run,
    id,
    title: "Nightly dependency audit",
    state: "COMPLETED",
    ...over,
  });

  it("renders no toggle when the whole group already fits", async () => {
    listRunsMock.mockResolvedValue([inGroup("run-c1"), inGroup("run-c2")]);
    renderScreen();
    await screen.findByRole("region", { name: "Nightly dependency audit" });
    expect(screen.queryByRole("button", { name: /^show fewer$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^show all/i })).not.toBeInTheDocument();
  });

  it("renders one when the group actually has more to show", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listRunsMock.mockResolvedValue(
      ["run-c1", "run-c2", "run-c3", "run-c4"].map((id) => inGroup(id)),
    );
    renderScreen();
    const toggle = await screen.findByRole("button", { name: "Show all 4" });
    await user.click(toggle);
    expect(screen.getByRole("button", { name: /^show fewer$/i })).toBeInTheDocument();
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

// The board's grouping axis is the run's TITLE — runs that share one are the
// same piece of work. It replaced grouping by state; see runs.tsx's titleGroups.
describe("RunsScreen — runs are grouped by title", () => {
  const titled = (id: string, title: string, over: Partial<AgentRun> = {}): AgentRun => ({
    ...run,
    id,
    title,
    ...over,
  });

  it("puts runs that share a title under one header, with per-state counts", async () => {
    listRunsMock.mockResolvedValue([
      titled("r1", "Nightly dependency audit"),
      titled("r2", "Nightly dependency audit", { state: "COMPLETED" }),
    ]);
    renderScreen();

    const group = await screen.findByRole("region", { name: "Nightly dependency audit" });
    // The header carries the count, and each card names its OWN work rather
    // than reprinting the title it already sits under.
    expect(within(group).getByText("2")).toBeInTheDocument();
    expect(within(group).getAllByText("Fix flaky auth tests")).toHaveLength(2);
  });

  // A group of one is not a group. Untitled runs — every row created before
  // titles existed, every CLI run, every system run — must keep rendering, and
  // by their task, which is the only name they have ever had.
  it("leaves a lone titled run and every untitled run ungrouped, named by task", async () => {
    listRunsMock.mockResolvedValue([titled("r1", "One-off cleanup"), run]);
    renderScreen();

    await waitFor(() => expect(listRunsMock).toHaveBeenCalled());
    expect(screen.queryByRole("region", { name: "One-off cleanup" })).not.toBeInTheDocument();
    // Ungrouped cards name themselves: the titled one by its title, the
    // untitled legacy row by its task.
    expect(await screen.findByText("One-off cleanup")).toBeInTheDocument();
    expect(screen.getByText("Fix flaky auth tests")).toBeInTheDocument();
  });

  // The regression this whole fallback exists for: an interactive run carries
  // NO task at all now, so a card reading it directly renders a bare dash.
  it("names an interactive run with no task by its title", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Debug the payments box", task: "", interactive: true },
    ]);
    renderScreen();
    expect(await screen.findByText("Debug the payments box")).toBeInTheDocument();
  });
});

// ── mock M2: the pinned "Needs you" lane ────────────────────────────────────
// The board used to flatten held approvals and failures into one amber
// treatment. They are not one thing: an approval is a REQUEST (someone is
// waiting on you) and a failure is a REPORT (something is over). The request
// pins to a lane at the top; the report stays with the work it belongs to.
describe("RunsScreen board — the pinned Needs-you lane", () => {
  const held = (runId: string) => ({
    id: `a-${runId}`,
    run_id: runId,
    kind: "egress_domain" as const,
    requested_scope: { host: "held.example", mode: "wait_for_review" },
    state: "PENDING" as const,
    requested_at: new Date().toISOString(),
  });

  it("pins a run whose sandbox is held — the run state alone never says so", async () => {
    listRunsMock.mockResolvedValue([{ ...run, id: "r1", task: "Rotate the staging credentials" }]);
    listApprovalsMock.mockResolvedValue([held("r1")]);
    renderScreen();

    const lane = await screen.findByRole("region", { name: "Needs you" });
    expect(within(lane).getByText("Rotate the staging credentials")).toBeInTheDocument();
    // …and it states what is waiting, rather than a sentence restating the state.
    expect(within(lane).getByText("1 waiting · sandbox held")).toBeInTheDocument();
  });

  it("a run is in the lane XOR its title group — never rendered twice", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Nightly audit", task: "Held step", state: "WAITING_FOR_CONFIRMATION" },
      { ...run, id: "r2", title: "Nightly audit", task: "Quiet step" },
      { ...run, id: "r3", title: "Nightly audit", task: "Third step" },
    ]);
    renderScreen();

    // Identity by short id, because a pinned card is OUT of its group and so
    // names itself by title again — the same rule any ungrouped card follows.
    const lane = await screen.findByRole("region", { name: "Needs you" });
    const group = screen.getByRole("region", { name: "Nightly audit" });
    expect(within(lane).getByTitle("r1")).toBeInTheDocument();
    expect(within(group).queryByTitle("r1")).not.toBeInTheDocument();
    expect(screen.getAllByTitle("r1")).toHaveLength(1);
    // The group keeps the rest, and its count follows the run that left.
    expect(within(group).getByText("Quiet step")).toBeInTheDocument();
    expect(within(group).getByText("2")).toBeInTheDocument();
  });

  it("a failure is a report: it stays in its group and never pins to the lane", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Nightly audit", task: "Broken step", state: "FAILED" },
      { ...run, id: "r2", title: "Nightly audit", task: "Quiet step" },
    ]);
    renderScreen();

    const group = await screen.findByRole("region", { name: "Nightly audit" });
    expect(within(group).getByText("Broken step")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Needs you" })).not.toBeInTheDocument();
    // The state word e2e pins is still in the DOM on the card's second row —
    // twice inside a group, because the header carries per-state counts too.
    expect(within(group).getAllByText("Failed")).toHaveLength(2);
  });

  it("no lane at all when nothing is asking — the heading is not permanent chrome", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    expect(screen.queryByRole("region", { name: "Needs you" })).not.toBeInTheDocument();
  });

  it("the table has no lane: a held run is still in its group there", async () => {
    const user = userEvent.setup();
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Nightly audit", task: "Held step", state: "WAITING_FOR_CONFIRMATION" },
      { ...run, id: "r2", title: "Nightly audit", task: "Quiet step" },
    ]);
    renderScreen();
    await screen.findByRole("region", { name: "Needs you" });

    await user.click(screen.getByRole("button", { name: /^table$/i }));
    expect(screen.queryByRole("region", { name: "Needs you" })).not.toBeInTheDocument();
    expect(screen.getAllByText("Held step")).toHaveLength(1);
    expect(screen.getByText("Quiet step")).toBeInTheDocument();
  });

  // The board must not lose its runs because the approvals endpoint is having
  // a bad day — it just loses the held join.
  it("a failing approvals call still renders the board", async () => {
    listApprovalsMock.mockRejectedValue(new Error("403"));
    renderScreen();
    expect(await screen.findByText("Fix flaky auth tests")).toBeInTheDocument();
  });
});

// mock M2's honest fallbacks: nothing on the card is invented.
describe("RunsScreen board — an ephemeral run names itself honestly", () => {
  it("falls back to its mode for the headline and says what the empty repo slot is", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "", task: "", repo: "", workspace_path: "", interactive: true },
    ]);
    renderScreen();

    expect(await screen.findByText("Interactive session")).toBeInTheDocument();
    expect(screen.getByText("Ephemeral scratch — no repo")).toBeInTheDocument();
  });
});
