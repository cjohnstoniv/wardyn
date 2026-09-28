/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Round 2 review residual on F4: nothing submitted the composer, so
// `source: "composer"` (runs-composer.tsx:45) could be deleted and every
// existing suite stayed green (FINAL-PR-1253.md round 2, F4). This pins the
// composer's own launch, end to end: it must reach New run's route state
// with BOTH the typed task and `source: "composer"` — the flag New run
// reads to skip the clone banner's false copy.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

vi.mock("../../../lib/use-workspace-list", () => ({
  useWorkspaceList: () => ({ workspaces: [], loading: false, error: false, reload: vi.fn() }),
}));

import { RunsComposer } from "./runs-composer";
import type { RunPrefill } from "../new-run/wizard-types";

function LocationProbe() {
  const state = useLocation().state as { prefill?: RunPrefill } | null;
  return <div data-testid="prefill">{JSON.stringify(state?.prefill ?? null)}</div>;
}

describe("RunsComposer — 'Start a run' (review F4)", () => {
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
