/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PhaseRail } from "./phase-rail";
import { STEP_LABEL, type SetupStepId, type StepBadge } from "./steps";

const BADGES: Record<SetupStepId, StepBadge> = {
  environment: { text: "Ready · 2 of 3 barriers", tone: "success" },
  provider: { text: "Ready", tone: "success" },
  "sealed-box": { text: "Optional", tone: "neutral" },
  "fail-then-approve": { text: "Optional", tone: "neutral" },
  "held-at-the-door": { text: "Optional", tone: "neutral" },
  "lines-that-cant-be-crossed": { text: "Optional", tone: "neutral" },
  host_proxy: { text: "Optional", tone: "neutral" },
  scm_provider: { text: "Configured", tone: "success" },
  artifact_repo: { text: "Optional", tone: "neutral" },
  workspaces: { text: "In progress", tone: "info" },
  credentials: { text: "Optional", tone: "neutral" },
  review: { text: "Review what's left", tone: "neutral" },
  launch: { text: "Set up the essentials first", tone: "neutral" },
};

const DONE: Record<SetupStepId, boolean> = {
  environment: true,
  provider: true,
  "sealed-box": false,
  "fail-then-approve": false,
  "held-at-the-door": false,
  "lines-that-cant-be-crossed": false,
  host_proxy: false,
  scm_provider: false,
  artifact_repo: false,
  workspaces: false,
  credentials: false,
  review: false,
  launch: false,
};

// The compact icon rail (lg-only) renders every step unconditionally (CSS-hidden,
// not DOM-absent — jsdom doesn't apply Tailwind's responsive `hidden`), so its
// buttons share accessible names with the full rail's. Scope queries to the full
// rail's nav landmark, same as production CSS would at xl+.
function renderRail(current: SetupStepId, onSelect = vi.fn()) {
  cleanup(); // some tests render twice (collapsed vs. expanded) to compare
  render(<PhaseRail current={current} badges={BADGES} done={DONE} onSelect={onSelect} />);
  return within(screen.getByRole("navigation", { name: /setup steps/i }));
}

describe("PhaseRail", () => {
  it("a full-rail step button carries both the frozen label and its badge text", () => {
    const rail = renderRail("environment");
    const btn = rail.getByRole("button", { name: /scm provider/i });
    expect(within(btn).getByText("Configured")).toBeInTheDocument();
  });

  it("renders all 13 frozen labels as buttons in the full rail", () => {
    // current inside the corporate phase so it auto-expands and all 13 show
    // (the 4 Demos sub-steps + the rest).
    const rail = renderRail("host_proxy");
    for (const label of Object.values(STEP_LABEL)) {
      expect(rail.getByRole("button", { name: new RegExp(label, "i") })).toBeInTheDocument();
    }
  });

  it("corporate group is collapsed by default but auto-expands when current is inside it", () => {
    const collapsed = renderRail("environment");
    expect(collapsed.queryByRole("button", { name: /host proxy/i })).not.toBeInTheDocument();

    const expanded = renderRail("host_proxy");
    expect(expanded.getByRole("button", { name: /host proxy/i })).toBeInTheDocument();
  });

  it("clicking a step button calls onSelect with its id", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const rail = renderRail("environment", onSelect);
    await user.click(rail.getByRole("button", { name: /scm provider/i }));
    expect(onSelect).toHaveBeenCalledWith("scm_provider");
  });

  it('renders "2/2" for the essentials phase counter once both its steps are done', () => {
    const rail = renderRail("environment");
    expect(rail.getByText("2/2")).toBeInTheDocument();
  });

  it('all-optional phases read "all optional", never a counter that cannot fill', () => {
    const rail = renderRail("environment");
    // Three phases are made only of optional steps and so read "all optional":
    // Demos (single optional step), the corporate group, and "Your work"
    // (scm/workspaces/credentials — credentials is done-pinned false by the
    // honesty law, so a 0/3 counter there could structurally never reach 3/3).
    expect(rail.getAllByText("all optional")).toHaveLength(3);
    expect(rail.queryByText("0/3")).not.toBeInTheDocument();
    expect(rail.getByText("0/2")).toBeInTheDocument(); // Finish still counts
  });

  it("marks only the active step aria-current=step, and no button anywhere uses aria-pressed", () => {
    const rail = renderRail("provider");
    expect(rail.getByRole("button", { name: /model\/harness provider/i })).toHaveAttribute(
      "aria-current",
      "step",
    );
    expect(rail.getByRole("button", { name: /^environment/i })).not.toHaveAttribute("aria-current");

    for (const btn of screen.getAllByRole("button")) {
      expect(btn).not.toHaveAttribute("aria-pressed");
    }
  });
});
