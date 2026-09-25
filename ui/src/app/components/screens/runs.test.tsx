/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

// fix: the board's "Kill run" action must ask for confirmation before it
// fires api.killRun — the same AlertDialog gate as the identical action on
// Run Detail. These tests pin that the board asks first.

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
import { AttentionPublisherProvider } from "../../lib/attention-context";
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

function renderScreen(role?: Role, path = "/runs") {
  const tree = <RunsScreen />;
  return render(
    <MemoryRouter initialEntries={[path]}>
      {role ? <RoleProvider role={role}>{tree}</RoleProvider> : tree}
    </MemoryRouter>,
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
    expect(screen.getByRole("link", { name: /guided tour/i })).toHaveAttribute("href", "/admin/setup");
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
    // R6 F001 (docs/design/ui-batch3-mock.md): the command must be one that
    // EXISTS. `wardyn setup` has status/detect-proxy/proxy-relay/wall/vault —
    // there is no `fence` subcommand and there cannot be one (CC1 Fence is the
    // baseline tier nothing installs), and the banner's trigger is an empty
    // confinement_classes (an unreachable runner), not a missing tier.
    expect(screen.getByText("wardyn setup status")).toBeInTheDocument();
    expect(screen.queryByText(/setup fence/)).toBeNull();
    // ...and no sudo: the CLI talks to the daemon with the operator's token, so
    // root buys nothing and would read root's config instead of the operator's.
    expect(screen.queryByText(/sudo/)).toBeNull();
    // The copy-to-clipboard affordance beside it survives the reword.
    expect(screen.getByRole("button", { name: "Copy setup command" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeInTheDocument();
    // The board itself still renders underneath — the banner sits above it,
    // it doesn't replace the screen.
    expect(await screen.findByText(run.task)).toBeInTheDocument();
  });

  // /setup/status is a full ListRuns plus a shell-out host sweep. It is
  // polled only while there is actually something to watch for — the
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
// state. New run is its own page, so the same intent redirects to it — and
// the redirect replaces the history entry, so Back goes where the operator
// came from instead of bouncing through this screen and redirecting again.
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

// M-7 (admin-member-modes-design.md §6): the description is keyed on the
// VIEW, not the role — the list is already server-scoped to the user's own
// runs (handleListRuns's creator-pager branch) either way, so this only pins
// the copy that says so plainly.
describe("RunsScreen — user vs admin view count line", () => {
  it("a user (always the user view) sees \"Your runs · N\"", async () => {
    renderScreen("user");
    expect(await screen.findByText("Your runs · 1")).toBeInTheDocument();
  });

  it("an admin on /admin/runs keeps the unscoped description", async () => {
    renderScreen("admin", "/admin/runs");
    expect(await screen.findByText(/Every run, live/)).toBeInTheDocument();
    expect(screen.queryByText(/Your runs ·/)).not.toBeInTheDocument();
  });

  // The defect this pins: an admin reading /runs (the user view) must see the
  // SAME "Your runs" a member sees — the old role-keyed copy showed the
  // admin's unscoped description here too, which is wrong once the view can
  // differ from the role.
  it("an admin on /runs (the user view) ALSO sees \"Your runs · N\"", async () => {
    renderScreen("admin", "/runs");
    expect(await screen.findByText("Your runs · 1")).toBeInTheDocument();
    expect(screen.queryByText(/Every run, live/)).not.toBeInTheDocument();
  });
});

// fix: the board card and the table row must not be role="button" tabIndex={0}
// containers directly nesting the real Attach/Review/kebab <button>s inside
// them — that is an invalid, double-nested interactive-widget structure (a11y
// blocker). Mouse click-to-open stays via plain onClick; keyboard/AT users
// reach the same action via the existing "Open detail" menu item.
describe("RunsScreen — row/card container is not itself a redundant role=button widget (a11y)", () => {
  it("board density: the card is not exposed as its own button widget nesting the real action buttons", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    // If the card were role="button", its accessible name would compute from
    // its text content — including the task text — and this query would
    // match the outer card div itself.
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

// fix: the loading skeleton must match the active density — rendering the
// Board card-grid shape in Table density flashes the wrong skeleton on every
// manual Refresh / re-navigation.
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

// F1-F7: the table cap must not budget headers GLOBALLY against the data cap
// (`flat.slice(0, cap + groups.length)`), or a header could land exactly on
// the cut and render as the LAST row with nothing under it.
describe("RunsScreen table — the cap never ends on an orphan group header", () => {
  // ticket: F1-F7
  it("caps at the data-row count, not the header+data count, and never leaves a trailing header", async () => {
    const inGroup = (id: string, title: string): AgentRun => ({ ...run, id, title, state: "COMPLETED" });
    // Group A alone is exactly the default cap (25) — the classic trigger: a
    // global slice lands exactly on Group B's header.
    const groupA = Array.from({ length: 25 }, (_, i) => inGroup(`a${i}`, "Group A"));
    const groupB = Array.from({ length: 25 }, (_, i) => inGroup(`b${i}`, "Group B"));
    listRunsMock.mockResolvedValue([...groupA, ...groupB]);
    const user = userEvent.setup();
    renderScreen();
    await user.click(screen.getByRole("button", { name: /^table$/i }));
    await screen.findByText("Group A");

    // Group B's header must not render with nothing under it.
    expect(screen.queryByText("Group B")).not.toBeInTheDocument();
    // The footer's count matches what's actually on screen (25 run rows).
    expect(screen.getByText(/25 of 50/)).toBeInTheDocument();
  });
});

// F1-F10: "Refresh now" must not call `load`, which flips status to "loading"
// and unmounts the WHOLE toolbar (search input, focus and all) for a round
// trip the board already runs every POLL_MS in the background.
describe("RunsScreen — Refresh now stays on the background path", () => {
  // ticket: F1-F10
  it("never blanks the toolbar into a skeleton while the manual refresh is in flight", async () => {
    // `load` flips status to "loading" SYNCHRONOUSLY, unmounting the whole
    // `status === "ready"` branch — search input, focus, toolbar and board —
    // for the round trip; "Refresh now" must stay off this path. Hold the
    // refresh's own fetch open so the mid-flight DOM is inspectable, not just
    // the settled result.
    let resolveRefresh!: (v: unknown[]) => void;
    listRunsMock
      .mockResolvedValueOnce([run]) // the initial foreground load
      .mockImplementationOnce(() => new Promise((res) => { resolveRefresh = res; }));
    const user = userEvent.setup();
    renderScreen();
    const search = await screen.findByPlaceholderText(/search runs/i);

    const refreshBtn = screen.getByRole("button", { name: "Refresh now" });
    await user.click(refreshBtn);
    // Still in flight: the SAME search input node is still mounted — `load`
    // unmounting the ready branch (skeleton in its place) would hand back a
    // brand new element here instead.
    expect(screen.getByPlaceholderText(/search runs/i)).toBe(search);
    // R-7: NOT disabled — a disabled element isn't focusable, so a real
    // browser would blur the just-clicked button to <body> for the round
    // trip, the exact focus loss this whole finding exists to fix.
    expect(refreshBtn).not.toBeDisabled();

    resolveRefresh([run]);
    await waitFor(() => expect(listRunsMock).toHaveBeenCalledTimes(2));
  });
});

// R-1: pausing App.tsx's own attention-badge poll on /runs (X3-F13) only
// keeps both nav badges live if something else feeds them while parked there
// — this is that something else. Off the SAME unfiltered fetch the paused
// poll would have used, not the search/facet-filtered board state.
describe("RunsScreen — publishes its counts up for the shell's paused badge poll (R-1)", () => {
  it("calls the setter with the pending-approval count and the attention count, off the unfiltered fetch", async () => {
    listRunsMock.mockResolvedValue([run]); // RUNNING
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: run.id,
        kind: "tool_call", // unconditionally "held" — a real attention case, not a vacuous 0/0
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { tool: "r1.exec" },
      },
    ]);
    const publish = vi.fn();
    render(
      <MemoryRouter>
        <AttentionPublisherProvider value={publish}>
          <RunsScreen />
        </AttentionPublisherProvider>
      </MemoryRouter>,
    );

    await waitFor(() => expect(publish).toHaveBeenCalledWith({ pendingApprovals: 1, attentionCount: 1 }));
  });
});

// fix: the board's collapse toggle must not be gated on a count coincidence
// like `shownCount >= done.length` — true even when nothing had ever been
// expanded, whenever every group happened to fit within GROUP_PREVIEW, which
// renders a dead "Show fewer" button wired to a no-op. The board groups by
// title now, not by outcome, but the invariant is the same and rides on the
// same GROUP_PREVIEW: never render a control that has nothing to do.
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
