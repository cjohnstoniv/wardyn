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
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

const reloadMock = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/use-workspace-list", () => ({
  useWorkspaceList: () => ({ workspaces: [], loading: false, error: false, reload: reloadMock }),
}));

import { RunsComposer } from "./runs-composer";
import type { RunPrefill } from "../new-run-flow/draft/clone-prefill";

function LocationProbe() {
  const state = useLocation().state as { prefill?: RunPrefill } | null;
  return <div data-testid="prefill">{JSON.stringify(state?.prefill ?? null)}</div>;
}

describe("RunsComposer — 'Start a run'", () => {
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
