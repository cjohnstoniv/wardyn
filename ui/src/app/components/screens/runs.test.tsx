/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
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
      // Mirrors the mock's own worked example (13 shown rows + 4
      // older + 1 killed = "Your runs · 18") — here 3 shown + 4 + 1 = 8.
      hiddenOlder: 4,
      hiddenKilled: 1,
    });
    renderScreen();
    await screen.findByText("Needs a look");
    // "Your runs · N" counts every run the caller owns, before
    // filters/ageing — the shown rows plus the two hidden counts, not just
    // runsList.length (home-runs-1197-packet.html:672, design §2.1).
    expect(screen.getByText("Your runs · 8")).toBeInTheDocument();
    expect(screen.getByText("Needs your approval")).toBeInTheDocument();
    expect(screen.getByText("Still running")).toBeInTheDocument();
    expect(screen.getAllByText("Running", { exact: true }).length).toBeGreaterThan(0);
    expect(screen.getByText("All done")).toBeInTheDocument();
    expect(screen.getByText("Completed", { exact: true })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Needs you/ })).toBeInTheDocument();

    // the design's fixed section order (H-1/H-6) — Needs you,
    // then Running, then Ended today — as a DOM-order assertion, not just
    // "each one exists somewhere". The reviewer's swap mutation
    // (runs.tsx: Needs/Running lines) survived every existing test because
    // none checked order.
    const headings = screen.getAllByRole("heading").map((h) => h.textContent);
    const needsIdx = headings.findIndex((t) => t?.includes("Needs you"));
    const runningIdx = headings.findIndex((t) => t === "Running");
    const endedIdx = headings.findIndex((t) => t?.includes("Ended today"));
    expect(needsIdx).toBeGreaterThanOrEqual(0);
    expect(needsIdx).toBeLessThan(runningIdx);
    expect(runningIdx).toBeLessThan(endedIdx);
  });

  it("a lease-ended row shows the square glyph and reads 'ended … · ran …', not 'started …'", async () => {
    const startedAt = new Date(Date.now() - 3600_000).toISOString();
    const lostAt = new Date(Date.now() - 1800_000).toISOString();
    listRunsFilteredMock.mockResolvedValue({
      runs: [
        run({
          id: "lost1",
          task: "Lease ended mid-flight",
          state: "RUNNING",
          created_at: startedAt,
          lost_reason: "ended",
          lost_at: lostAt,
        }),
      ],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    await screen.findByText("Lease ended mid-flight");
    expect(screen.getByText("Ended at its end time")).toBeInTheDocument();
    // `ended_at` is absent on a lease-ended run — lost_at stands in
    // for it, or the meta line falls back to "started …" like a live run.
    expect(screen.getByTitle(/^acme\/widgets · ended .* · ran /)).toBeInTheDocument();
    expect(screen.queryByTitle(/started/)).toBeNull();
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

  it("a refetch (search) keeps the filter bar mounted and its input focused — only the rows region shows a skeleton", async () => {
    listRunsFilteredMock.mockResolvedValueOnce({
      runs: [run()],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    await screen.findByText("Fix flaky auth tests");

    const search = screen.getByLabelText("Search runs");
    search.focus();
    expect(search).toHaveFocus();

    const gate = pending<{ runs: AgentRun[]; truncated: boolean; hiddenOlder: number; hiddenKilled: number }>();
    listRunsFilteredMock.mockReturnValueOnce(gate.promise);
    fireEvent.change(search, { target: { value: "a" } });

    // The refetch is in flight — the SAME input (same node) is still here
    // and still focused; the reviewer's evidence for this bug was exactly
    // the opposite (sawSkeleton true, sameNode false, focused false).
    expect(screen.getByLabelText("Search runs")).toBe(search);
    expect(search).toHaveFocus();
    expect(search).toHaveValue("a");
    // The rows region is a skeleton, not the stale rows and not the whole
    // page replaced.
    expect(screen.getByLabelText("Loading runs")).toBeInTheDocument();

    gate.resolve({ runs: [], truncated: false, hiddenOlder: 0, hiddenKilled: 0 });
    await waitFor(() => expect(screen.queryByLabelText("Loading runs")).not.toBeInTheDocument());
    expect(screen.getByLabelText("Search runs")).toBe(search);
  });

  it("a slower, now-stale response cannot overwrite a newer one", async () => {
    listRunsFilteredMock.mockResolvedValueOnce({
      runs: [run()],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    renderScreen();
    await screen.findByText("Fix flaky auth tests");
    const search = screen.getByLabelText("Search runs");

    const gateA = pending<{ runs: AgentRun[]; truncated: boolean; hiddenOlder: number; hiddenKilled: number }>();
    listRunsFilteredMock.mockReturnValueOnce(gateA.promise);
    fireEvent.change(search, { target: { value: "a" } });

    const gateB = pending<{ runs: AgentRun[]; truncated: boolean; hiddenOlder: number; hiddenKilled: number }>();
    listRunsFilteredMock.mockReturnValueOnce(gateB.promise);
    fireEvent.change(search, { target: { value: "ab" } });

    // The NEWER request ("ab") settles first.
    gateB.resolve({
      runs: [run({ id: "b", task: "ab result" })],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    await screen.findByText("ab result");

    // The OLDER request's response lands late — it must be ignored, not
    // overwrite what "ab" already rendered.
    gateA.resolve({
      runs: [run({ id: "a", task: "a result" })],
      truncated: false,
      hiddenOlder: 0,
      hiddenKilled: 0,
    });
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText("a result")).not.toBeInTheDocument();
    expect(screen.getByText("ab result")).toBeInTheDocument();
  });

  it("workspace-detail's 'Start a run' (openNewRun route state) redirects to /runs/new — the REAL RunsScreen, not a stub", async () => {
    function LocationProbe() {
      return <div data-testid="location">{useLocation().pathname}</div>;
    }
    render(
      <MemoryRouter initialEntries={[{ pathname: "/runs", state: { openNewRun: true } }]}>
        <Routes>
          <Route path="/runs" element={<RunsScreen />} />
          <Route path="/runs/new" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/runs/new"));
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
