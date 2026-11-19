/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// This pins the
// composer's own launch, end to end: it must reach New run's route state
// with BOTH the typed task and `source: "composer"` — the flag New run
// reads to skip the clone banner's false copy.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { RUNS_COMPOSER } from "../../wardyn/copy/runs-landing";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

const reloadMock = vi.hoisted(() => vi.fn());
const listState = vi.hoisted(() => ({ loading: false, error: false }));
vi.mock("../../../lib/use-workspace-list", () => ({
  useWorkspaceList: () => ({ workspaces: [], loading: listState.loading, error: listState.error, reload: reloadMock }),
}));

import { RunsComposer } from "./runs-composer";
import type { RunPrefill } from "../new-run-flow/draft/clone-prefill";

function LocationProbe() {
  const state = useLocation().state as { prefill?: RunPrefill } | null;
  return <div data-testid="prefill">{JSON.stringify(state?.prefill ?? null)}</div>;
}

describe("RunsComposer — 'Start a run'", () => {
  it("a failed workspace read says the list is unavailable and Try again reads it again", () => {
    reloadMock.mockClear();
    listState.error = true;
    try {
      render(<MemoryRouter><RunsComposer /></MemoryRouter>);
      expect(screen.getByRole("alert")).toHaveTextContent(RUNS_COMPOSER.WORKSPACES_UNAVAILABLE);
      fireEvent.click(screen.getByRole("button", { name: RUNS_COMPOSER.WORKSPACES_RETRY }));
      expect(reloadMock).toHaveBeenCalledTimes(2); // the mount read, then the retry
    } finally {
      listState.error = false;
    }
  });

  it("says the workspaces are loading, and shows no failure while they do", () => {
    listState.loading = true;
    listState.error = true;
    try {
      render(<MemoryRouter><RunsComposer /></MemoryRouter>);
      expect(screen.getByRole("status")).toHaveTextContent(RUNS_COMPOSER.WORKSPACES_LOADING);
      // The note is outside the label, so the picker keeps its own name and gains the note as its description.
      const picker = screen.getByRole("combobox", { name: RUNS_COMPOSER.WORKSPACE });
      expect(picker).toHaveAccessibleDescription(RUNS_COMPOSER.WORKSPACES_LOADING);
      expect(screen.queryByRole("alert")).toBeNull();
    } finally {
      listState.loading = false;
      listState.error = false;
    }
  });

  it("shows no note when the list loaded", () => {
    render(<MemoryRouter><RunsComposer /></MemoryRouter>);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("asks for the workspace list on mount, since the hook fetches nothing until asked", () => {
    reloadMock.mockClear();
    render(<MemoryRouter><RunsComposer /></MemoryRouter>);
    expect(reloadMock).toHaveBeenCalledTimes(1);
  });

  it("sends source: 'composer' alongside the typed task, so New run skips the clone banner", async () => {
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <Routes>
          <Route path="/runs" element={<RunsComposer />} />
          <Route path="/runs/new" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>,
    );
    // getByLabelText would also match the <form aria-label="Start a run">
    // itself (RTL's own aria-label rule), so scope to the textbox role.
    fireEvent.change(screen.getByRole("textbox", { name: "Start a run" }), {
      target: { value: "Fix the flaky test" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));

    const prefill = JSON.parse(await screen.findByTestId("prefill").then((el) => el.textContent ?? "null"));
    expect(prefill).toMatchObject({ source: "composer", state: { task: "Fix the flaky test" } });
  });
});
