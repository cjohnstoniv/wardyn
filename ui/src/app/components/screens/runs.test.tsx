/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

const listRunsFilteredMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { listRunsFiltered: (...a: unknown[]) => listRunsFilteredMock(...a) },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../lib/use-workspace-list", () => ({
  useWorkspaceList: () => ({ workspaces: [], loading: false, error: false, reload: vi.fn() }),
}));

import { RunsScreen } from "./runs";
import { RoleProvider, type Role } from "../wardyn/operator-context";
import { baseStatus } from "../../lib/test-fixtures";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
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
  ...over,
});

function renderScreen(role?: Role, path = "/runs") {
  const tree = <RunsScreen />;
  return render(
    <MemoryRouter initialEntries={[path]}>
      {role ? <RoleProvider role={role}>{tree}</RoleProvider> : tree}
    </MemoryRouter>,
  );
}

function pending<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

beforeEach(() => {
  listRunsFilteredMock.mockReset();
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus({ has_runs: true }));
  listRunsFilteredMock.mockResolvedValue({ runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 });
});

describe("RunsScreen — page states (design.md §2.1)", () => {
  it("loading: an aria-busy skeleton renders before the fetch settles", async () => {
    const gate = pending<{ runs: AgentRun[]; truncated: boolean; hiddenOlder: number; hiddenKilled: number }>();
    listRunsFilteredMock.mockReturnValue(gate.promise);
    renderScreen();
    expect(screen.getByLabelText("Loading runs")).toBeInTheDocument();
    gate.resolve({ runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 });
    await waitFor(() => expect(screen.queryByLabelText("Loading runs")).not.toBeInTheDocument());
  });

  it("error, then Retry recovers", async () => {
    listRunsFilteredMock.mockRejectedValueOnce(new Error("boom"));
    renderScreen();
    await screen.findByText("Wardyn isn't answering. Try again.");
    listRunsFilteredMock.mockResolvedValueOnce({
      runs: [run()],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText("Fix flaky auth tests");
  });

  it("first run, User view (member): the composer plus the member empty state", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ has_runs: false }));
    renderScreen("user");
    await screen.findByText("Runs you launch appear here");
    expect(screen.getByRole("form", { name: "Start a run" })).toBeInTheDocument();
  });

  it("first run, Admin view: the inline empty state, no composer", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ has_runs: false }));
    renderScreen(undefined, "/admin/runs");
    await screen.findByText("No runs yet");
    expect(screen.queryByRole("form", { name: "Start a run" })).not.toBeInTheDocument();
  });

  it("all quiet: nothing needs you and nothing is running, Ended sections still show", async () => {
    // has_runs=true with a truly empty AND nothing-hidden window can't occur in
    // real data (a run is always either live or accounted for by a hidden
    // count) — a completed-today run is the realistic "quiet" fixture: no
    // decide/running rows, but the Ended today section still renders below
    // the dashed line, exactly as design.md's own "then Ended sections" says.
    listRunsFilteredMock.mockResolvedValue({
      runs: [run({ id: "e1", task: "Finished earlier", state: "COMPLETED", ended_at: new Date().toISOString() })],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    await screen.findByText("Nothing needs you and nothing is running.");
    expect(screen.getByText("Finished earlier")).toBeInTheDocument();
  });

  it("populated: every row kind renders its own status word (design.md §2.2)", async () => {
    listRunsFilteredMock.mockResolvedValue({
      runs: [
        run({ id: "d1", task: "Needs a look", attention: { kind: "approval", by: "you", pending: 1 } }),
        run({ id: "r1", task: "Still running", state: "RUNNING" }),
        run({
          id: "e1",
          task: "All done",
          state: "COMPLETED",
          ended_at: new Date().toISOString(),
        }),
      ],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    await screen.findByText("Needs a look");
    expect(screen.getByText("Needs your approval")).toBeInTheDocument();
    expect(screen.getByText("Still running")).toBeInTheDocument();
    expect(screen.getAllByText("Running", { exact: true }).length).toBeGreaterThan(0);
    expect(screen.getByText("All done")).toBeInTheDocument();
    expect(screen.getByText("Completed", { exact: true })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Needs you/ })).toBeInTheDocument();
  });

  it("the ageing note hides a 2-day-old killed run, and Include killed shows it", async () => {
    listRunsFilteredMock.mockResolvedValue({
      runs: [run({ id: "r1", state: "RUNNING" })],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 1,
    });
    renderScreen();
    await screen.findByText(/1 killed run is hidden/);
    listRunsFilteredMock.mockResolvedValue({
      runs: [
        run({ id: "r1", state: "RUNNING" }),
        run({ id: "k1", task: "Killed 2 days ago", state: "KILLED", ended_at: new Date().toISOString() }),
      ],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    fireEvent.click(screen.getByRole("button", { name: "Include killed" }));
    await screen.findByText("Killed 2 days ago");
    await waitFor(() =>
      expect(listRunsFilteredMock).toHaveBeenLastCalledWith(expect.objectContaining({ includeKilled: true })),
    );
  });

  it("no match, then Clear restores the default filters", async () => {
    // The mock stands in for the server's own q= filtering (runs-api-filters
    // is what pins the real thing): populated with the default (no q), empty
    // once a query param is present — exactly the server behaviour a search
    // that matches nothing produces.
    listRunsFilteredMock.mockImplementation((f: { q?: string }) =>
      Promise.resolve(
        f.q
          ? { runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 }
          : { runs: [run()], truncated: false, hiddenOlder: 0, hiddenKilled: 0 },
      ),
    );
    renderScreen();
    await screen.findByText("Fix flaky auth tests");
    const search = await screen.findByLabelText("Search runs");
    fireEvent.change(search, { target: { value: "zzz-no-match" } });
    await screen.findByText("No runs match these filters.");
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await screen.findByText("Fix flaky auth tests");
  });

  it("'Earlier this week' expands on click and is aria-expanded", async () => {
    listRunsFilteredMock.mockResolvedValue({
      runs: [
        run({
          id: "e2",
          task: "From three days ago",
          state: "COMPLETED",
          ended_at: new Date(Date.now() - 3 * 24 * 3600_000).toISOString(),
        }),
      ],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    const toggle = await screen.findByRole("button", { name: /Earlier this week/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("From three days ago")).not.toBeInTheDocument();
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await screen.findByText("From three days ago");
  });
});
