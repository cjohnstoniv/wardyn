/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from runs.test.tsx (#195): that file was over the 800-line test gate.
// The first-run/empty-state, kill-confirm and route-redirect describes stay
// there; the board-grouping, Needs-you lane, and Admin-monitor (M-7) describes
// that only need the board/table rendering paths live here.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

const listRunsMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { listRuns: (...a: unknown[]) => listRunsMock(...a) },
}));
const listApprovalsMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a) },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

import { RunsScreen } from "./runs";
import { OperatorProvider, RoleProvider, type Role } from "../wardyn/operator-context";
import { RUN } from "../wardyn/copy";
import { OPEN_IN_USER_VIEW } from "../wardyn/copy/console-view";
import { baseStatus } from "../../lib/test-fixtures";
import { STARTING_UNSCHEDULABLE } from "./run-status-detail";
import { HttpError } from "../../lib/api/core";

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
  listApprovalsMock.mockReset();
  getSetupStatusMock.mockReset();
  listRunsMock.mockResolvedValue([run]);
  // Default: nothing parked on anything.
  listApprovalsMock.mockResolvedValue([]);
  // Non-blocking default: both barrier tiers available, nothing to re-check.
  getSetupStatusMock.mockResolvedValue(baseStatus({ ready: true }));
});

// fix: Runs rendered its "Live" auto-refresh indicator as plain muted text +
// a raw CircleDot icon, while Audit renders the same concept as the shared
// Chip pill primitive — two visual treatments for one concept. Runs now uses
// the same Chip.
describe("RunsScreen — Live indicator uses the shared Chip primitive (matches Audit)", () => {
  it("renders the Live copy inside the Chip pill (title carries the polling reason, like Audit's)", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    // #215: "Live" alone — "refreshes every 3s" narrated the polling
    // implementation; the title says the same thing without the number.
    const chip = screen.getByTitle("Refreshing on its own");
    expect(chip).toHaveTextContent("Live");
    expect(chip).not.toHaveTextContent("refreshes every");
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

  // #215 — "Other runs" replaces "Ungrouped", a data-model word. Shown only
  // when there is a real title group to distinguish it FROM, same rule as before.
  it("labels the loose section 'Other runs' once a real title group exists above it", async () => {
    listRunsMock.mockResolvedValue([
      titled("r1", "Nightly dependency audit"),
      titled("r2", "Nightly dependency audit", { state: "COMPLETED" }),
      { ...run, id: "r3", title: "", task: "A loose one-off run" },
    ]);
    renderScreen();
    await screen.findByRole("region", { name: "Nightly dependency audit" });
    expect(screen.getByText("Other runs")).toBeInTheDocument();
    expect(screen.queryByText("Ungrouped")).not.toBeInTheDocument();
  });
});

// #215 — a run opens from a link, in the product's vocabulary: the title is a
// real <a href> (board AND table), a failed run offers "Open" not "Review",
// the workspace facet/column says "Workspace" not "Repo", and the Live chip
// drops the polling detail.
describe("RunsScreen — a run opens from a link (#215)", () => {
  it("the board card's title is a real, keyboard-reachable <a href>", async () => {
    renderScreen();
    const link = await screen.findByRole("link", { name: "Fix flaky auth tests" });
    expect(link).toHaveAttribute("href", "/runs/run-1");
  });

  it("the table row's title is a real <a href> too — the same click-handler-on-a-div defect runs.tsx had", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /^table$/i }));
    const link = await screen.findByRole("link", { name: "Fix flaky auth tests" });
    expect(link).toHaveAttribute("href", "/runs/run-1");
    expect(screen.getByRole("columnheader", { name: "Workspace" })).toBeInTheDocument();
    expect(screen.queryByRole("columnheader", { name: "Repo" })).not.toBeInTheDocument();
  });

  it("the workspace facet says Workspace, not Repo", async () => {
    renderScreen();
    await screen.findByRole("button", { name: /run actions/i });
    expect(screen.getByRole("combobox", { name: "Workspace" })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Repo" })).not.toBeInTheDocument();
  });

  it("a failed run's card offers Open, not Review — it is a report, not a request", async () => {
    listRunsMock.mockResolvedValue([{ ...run, id: "r1", state: "FAILED" }]);
    renderScreen();
    expect(await screen.findByRole("button", { name: "Open" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Review" })).not.toBeInTheDocument();
  });
});

// Review finding on #638: every link off the Admin monitor must stay in the
// Admin view (/admin/runs/:id) — ViewGate's TWIN rule sends the plain
// /runs/:id path to the User view for a "url"-access install, and refuses it
// outright for an admin-only SSO token, so a board that ever linked there
// made the monitor reachable only by typing its URL.
describe("RunsScreen — admin board/table links stay in the Admin view (review finding, M-7)", () => {
  function LocationProbe() {
    const location = useLocation();
    return <span data-testid="path">{location.pathname}</span>;
  }

  it("the board card's title targets /admin/runs/:id when the board is /admin/runs", async () => {
    renderScreen(undefined, "/admin/runs");
    const link = await screen.findByRole("link", { name: "Fix flaky auth tests" });
    expect(link).toHaveAttribute("href", "/admin/runs/run-1");
  });

  it("the table row's title targets /admin/runs/:id too", async () => {
    const user = userEvent.setup();
    renderScreen(undefined, "/admin/runs");
    await user.click(await screen.findByRole("button", { name: /^table$/i }));
    const link = await screen.findByRole("link", { name: "Fix flaky auth tests" });
    expect(link).toHaveAttribute("href", "/admin/runs/run-1");
  });

  it("clicking a card on /admin/runs navigates to /admin/runs/:id, not /runs/:id", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/admin/runs"]}>
        <RunsScreen />
        <LocationProbe />
      </MemoryRouter>,
    );
    await user.click(await screen.findByTestId("run-card"));
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/admin/runs/run-1"));
  });
});

// mock M2: the pinned "Needs you" lane
// Held approvals and failures must not flatten into one amber treatment.
// They are not one thing: an approval is a REQUEST (someone is waiting on
// you) and a failure is a REPORT (something is over). The request pins to a
// lane at the top; the report stays with the work it belongs to.
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

  // The group header's danger signal follows the CARD RAIL, not needsAttention:
  // "monitoring" (a passive deny_with_review pending) is inside needsAttention
  // but no card paints it, so a red header sat over two ordinary RUNNING cards
  // with nothing red on them.
  it("a group whose only attention is a monitoring run gets no danger header", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Nightly audit", task: "Watched step" },
      { ...run, id: "r2", title: "Nightly audit", task: "Quiet step" },
    ]);
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "r1",
        kind: "egress_domain",
        requested_scope: { host: "unlisted.example" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ]);
    renderScreen();

    // The card states the passive pending, which proves the join landed — so
    // the header below is read with the signal present, not before it arrives.
    const group = await screen.findByRole("region", { name: "Nightly audit" });
    expect(await within(group).findByText("1 waiting")).toBeInTheDocument();
    expect(within(group).getByRole("heading", { name: "Nightly audit" }).className).not.toContain(
      "text-danger",
    );
  });

  it("a group holding a failed run still gets the danger header", async () => {
    listRunsMock.mockResolvedValue([
      { ...run, id: "r1", title: "Nightly audit", task: "Broken step", state: "FAILED" },
      { ...run, id: "r2", title: "Nightly audit", task: "Quiet step" },
    ]);
    renderScreen();

    const group = await screen.findByRole("region", { name: "Nightly audit" });
    expect(within(group).getByRole("heading", { name: "Nightly audit" }).className).toContain(
      "text-danger",
    );
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

// X3-F4 — the empty board was the operator's first-run funnel: a host barrier
// readout, an operator setup checklist and the demo grid, all of it either
// redacted-blank or unreachable for a member. A member's own empty board says
// what a member can do about it.
describe("RunsScreen — the member's empty board", () => {
  it("a member with no runs gets the member empty state, not the operator first-run funnel", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen("user");

    expect(await screen.findByText("Runs you launch appear here")).toBeInTheDocument();
    expect(screen.queryByText("No runs yet")).not.toBeInTheDocument();
    expect(screen.queryByText(/available on this host/)).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /new run/i })).toHaveAttribute("href", "/runs/new");
    expect(screen.getByRole("link", { name: /getting started/i })).toHaveAttribute("href", "/setup");
  });

  // The predicate must be `role !== "admin"`, not `role === "user"`.
  // /setup/status is redacted on !isOperator (internal/api/setup.go), and
  // isOperator is SUPER-admin only — so a security admin's status arrives with
  // checks [], secrets.present [] and the driver withheld, exactly like a
  // member's. Through `role === "user"` this tier would fall into the
  // operator funnel and read every withheld field as a fact: "Needs the
  // <name> secret" for secrets that may well exist, over two /setup deep
  // links that land on a Getting Started which ignores ?step. Every sibling
  // in this cluster (setupGateActive, GettingStarted) already uses the
  // three-valued form.
  it("a security admin with no runs gets the member empty state too — their /setup/status is redacted the same way", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen("security_admin");

    expect(await screen.findByText("Runs you launch appear here")).toBeInTheDocument();
    expect(screen.queryByText("No runs yet")).not.toBeInTheDocument();
    expect(screen.queryByText(/available on this host/)).not.toBeInTheDocument();
    expect(screen.queryByText(/needs the/i)).not.toBeInTheDocument();
  });

  it("negative control: an admin's empty board is the unchanged first-run funnel", async () => {
    listRunsMock.mockResolvedValue([]);
    renderScreen("admin");
    expect(await screen.findByText("No runs yet")).toBeInTheDocument();
    expect(screen.queryByText("Runs you launch appear here")).not.toBeInTheDocument();
  });
});

// 0.7.6 finding 6: the board is where a person scanning several runs can tell a
// pull from a scheduling failure without opening any of them. The FULL sentence
// here, not the header's short register — the cell has the width for it.
describe("RunsScreen — what a starting run is waiting on", () => {
  it("carries the substrate's sentence under the badge, on the card and in the table row", async () => {
    listRunsMock.mockResolvedValue([
      {
        ...run,
        id: "run-starting",
        state: "STARTING",
        task: "waiting on the cluster",
        status_detail: "pod: Unschedulable: 0/1 nodes are available: 1 node(s) had untolerated taint",
        status_reason: "Unschedulable",
      },
    ]);
    renderScreen();

    expect(await screen.findByText("waiting on the cluster")).toBeInTheDocument();
    expect(screen.getAllByText(STARTING_UNSCHEDULABLE).length).toBeGreaterThan(0);

    await userEvent.click(screen.getByRole("button", { name: "Table" }));
    expect(within(await screen.findByRole("table")).getByText(STARTING_UNSCHEDULABLE)).toBeInTheDocument();
  });

  // The server blanks status_detail for every run that is not STARTING, so the
  // board renders whatever it is sent — but a run carrying nothing must render
  // nothing, never an empty line under the badge.
  it("says nothing for a run with no reason", async () => {
    listRunsMock.mockResolvedValue([{ ...run, state: "STARTING" }]);
    renderScreen();

    expect(await screen.findByText("Fix flaky auth tests")).toBeInTheDocument();
    expect(screen.queryByText(STARTING_UNSCHEDULABLE)).toBeNull();
    expect(screen.queryByText(/^Waiting:/)).toBeNull();
  });
});

// getSetupStatus only ever rejects on a real 401 (setup.ts's own contract) —
// a lapsed session while this screen is mounted. loadSetupStatus used to have
// no .catch, so that rejection floated as an unhandled promise rejection
// right on the landing screen; vitest fails a run on an unhandled rejection
// on its own, so this test's whole job is to prove the mount survives the
// reject without one.
describe("RunsScreen — a lapsed session's 401 never floats unhandled (loadSetupStatus)", () => {
  it("mounts and boards the runs it already has when getSetupStatus rejects", async () => {
    getSetupStatusMock.mockRejectedValue(new HttpError(401, "Unauthorized"));
    renderScreen();

    // The board itself never depended on setup status to render runs it
    // already has — a rejected read must not blank it.
    expect(await screen.findByText("Fix flaky auth tests")).toBeInTheDocument();
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    // setupStatus stays null on a reject, same as "not answered yet" — never
    // the no-barrier banner off a read that never actually answered.
    expect(screen.queryByText(/no sandbox barrier/i)).toBeNull();
  });
});

// M-7 (modes-b §1: "Monitor, kill, decide. No New run and no relaunch. The
// admin's own run carries only a switch link.") — pinned on the route, since the
// screen keys all of it on the view it is mounted under.
describe("RunsScreen — /admin/runs is a monitor (M-7)", () => {
  const finished: AgentRun = { ...run, state: "COMPLETED" };
  const theirs: AgentRun = { ...run, id: "run-2", task: "Bump the lockfile", created_by: "bob@corp" };

  function renderAdmin(path: string) {
    return render(
      <MemoryRouter initialEntries={[path]}>
        <RoleProvider role="admin">
          <OperatorProvider operator principal="me">
            <RunsScreen />
          </OperatorProvider>
        </RoleProvider>
      </MemoryRouter>,
    );
  }

  async function openKebab() {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("button", { name: /run actions/i }));
    await screen.findByRole("menuitem", { name: /open detail/i });
  }

  it("a finished run's kebab offers no relaunch on /admin/runs", async () => {
    listRunsMock.mockResolvedValue([finished]);
    renderAdmin("/admin/runs");
    await openKebab();
    expect(screen.queryByRole("menuitem", { name: RUN.CLONE_CTA })).not.toBeInTheDocument();
  });

  it("negative control: the same admin on /runs keeps the relaunch", async () => {
    listRunsMock.mockResolvedValue([finished]);
    renderAdmin("/runs");
    await openKebab();
    expect(screen.getByRole("menuitem", { name: RUN.CLONE_CTA })).toBeInTheDocument();
  });

  it("an admin with no runs gets no New run, nor any other door that starts one", async () => {
    listRunsMock.mockResolvedValue([]);
    renderAdmin("/admin/runs");
    expect(await screen.findByText("No runs yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /new run/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /new run|try it without a repo|guided tour/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/available on this host/)).not.toBeInTheDocument();
  });

  it("the board marks the admin's own card (you) with the switch link; someone else's gets neither", async () => {
    listRunsMock.mockResolvedValue([run, theirs]);
    renderAdmin("/admin/runs");
    const cards = await screen.findAllByTestId("run-card");
    const mine = cards.find((c) => within(c).queryByText(run.task))!;
    const other = cards.find((c) => within(c).queryByText(theirs.task))!;
    expect(within(mine).getByText("me (you)")).toBeInTheDocument();
    expect(within(mine).getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeInTheDocument();
    expect(within(other).getByText("bob@corp")).toBeInTheDocument();
    expect(within(other).queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
  });

  it("the table marks the admin's own row the same way", async () => {
    listRunsMock.mockResolvedValue([run, theirs]);
    renderAdmin("/admin/runs");
    await userEvent.click(await screen.findByRole("button", { name: "Table" }));
    const mine = screen.getByRole("row", { name: new RegExp(run.task) });
    const other = screen.getByRole("row", { name: new RegExp(theirs.task) });
    expect(within(mine).getByText("me (you)")).toBeInTheDocument();
    expect(within(mine).getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeInTheDocument();
    expect(within(other).queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
  });

  it("the user view never marks a card or offers the switch link", async () => {
    listRunsMock.mockResolvedValue([run]);
    renderAdmin("/runs");
    await screen.findByText(run.task);
    expect(screen.queryByText("me (you)")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: OPEN_IN_USER_VIEW })).not.toBeInTheDocument();
  });
});
