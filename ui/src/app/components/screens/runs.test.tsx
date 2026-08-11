/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
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

function renderScreen() {
  return render(
    <MemoryRouter>
      <RunsScreen />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  listRunsMock.mockReset();
  killRunMock.mockReset();
  listRunsMock.mockResolvedValue([run]);
  killRunMock.mockResolvedValue(undefined);
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
