/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

// M16 fix: the board's "Kill run" action used to fire api.killRun immediately
// from the dropdown — no confirmation — unlike the identical action on Run
// Detail, which is AlertDialog-gated. These tests pin that the board now asks
// first.

const listRunsMock = vi.fn();
const killRunMock = vi.fn();
vi.mock("../../lib/api", () => ({
  api: {
    listRuns: (...a: unknown[]) => listRunsMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
  },
}));
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
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

describe("RunsScreen board — Kill run confirms before killing (M16)", () => {
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
});
