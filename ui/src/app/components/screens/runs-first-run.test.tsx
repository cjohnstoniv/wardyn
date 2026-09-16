/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// X3-F12: the Model provider checklist row's icon must agree with its own
// text, which already branches on llmReady ("Connected — …" vs "Not
// connected …"). Scoped narrowly — RunsFirstRun's other behaviour (the
// no-barrier banner, the demo grid) has its own coverage via runs.test.tsx /
// runs-first-run-demos.test.tsx.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunsFirstRun } from "./runs-first-run";
import type { Readiness } from "../../lib/readiness";

function readiness(overrides: Partial<Readiness> = {}): Readiness {
  return {
    ready: true,
    llmReady: false,
    barrierReady: true,
    barrierCount: 1,
    ...overrides,
  } as Readiness;
}

function renderIt(r: Readiness) {
  return render(
    <MemoryRouter>
      <RunsFirstRun readiness={r} confinementClasses={["CC1"]} secretNames={[]} onNewRun={() => {}} />
    </MemoryRouter>,
  );
}

describe("RunsFirstRun — model provider row icon (X3-F12)", () => {
  it("llmReady:false renders the dashed (untested) icon", () => {
    const { container } = renderIt(readiness({ llmReady: false }));
    const row = screen.getByText("Model provider").closest("li")!;
    expect(row.querySelector(".lucide-circle-dashed")).not.toBeNull();
    expect(row.querySelector(".lucide-circle-check")).toBeNull();
    expect(container).toBeInTheDocument(); // smoke: mounted at all
  });

  it("llmReady:true renders the check icon, not the dashed one", () => {
    renderIt(readiness({ llmReady: true }));
    const row = screen.getByText("Model provider").closest("li")!;
    expect(row.querySelector(".lucide-circle-check")).not.toBeNull();
    expect(row.querySelector(".lucide-circle-dashed")).toBeNull();
  });
});
