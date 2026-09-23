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
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunsFirstRun } from "./runs-first-run";
import type { Readiness } from "../../lib/readiness";
import type { ConfinementClass } from "../../lib/types";

function readiness(overrides: Partial<Readiness> = {}): Readiness {
  return {
    ready: true,
    llmReady: false,
    barrierReady: true,
    barrierCount: 1,
    ...overrides,
  } as Readiness;
}

function renderIt(r: Readiness, confinementClasses: ConfinementClass[] = ["CC1"]) {
  return render(
    <MemoryRouter>
      <RunsFirstRun readiness={r} confinementClasses={confinementClasses} secretNames={[]} onNewRun={() => {}} />
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

// #214 — the board's own route to the step that fixes it, the fourth surface
// carrying it alongside the shell banner, the top bar and New Run's Launch
// reason.
describe("RunsFirstRun — the Sandbox barrier row's route to Environment (#214)", () => {
  it("routes to the Environment step when no barrier is available", () => {
    renderIt(readiness({ barrierReady: false, barrierCount: 0 }), []);
    const row = screen.getByText("Sandbox barrier").closest("li")!;
    const link = within(row).getByRole("link", { name: /Set up a barrier/ });
    expect(link).toHaveAttribute("href", "/setup?step=environment");
  });

  it("carries no route when a barrier is available", () => {
    renderIt(readiness({ barrierReady: true, barrierCount: 1 }), ["CC1"]);
    const row = screen.getByText("Sandbox barrier").closest("li")!;
    expect(within(row).queryByRole("link")).toBeNull();
  });
});
