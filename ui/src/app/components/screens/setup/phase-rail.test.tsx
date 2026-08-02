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

  // The corporate-network steps used to be their own collapsible group. They now
  // live inside Essentials (before the model step) because they're prerequisites —
  // connecting a model and running the demos both need egress. So they are always
  // visible, never behind an expander.
  it("shows the corporate-network steps inline in Essentials, with no expander to open", () => {
    const rail = renderRail("environment");
    expect(rail.getByRole("button", { name: /host proxy/i })).toBeInTheDocument();
    expect(rail.getByRole("button", { name: /artifact redirect/i })).toBeInTheDocument();
    expect(rail.queryByRole("button", { name: /^corporate network$/i })).not.toBeInTheDocument();
  });

  it("clicking a step button calls onSelect with its id", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const rail = renderRail("environment", onSelect);
    await user.click(rail.getByRole("button", { name: /scm provider/i }));
    expect(onSelect).toHaveBeenCalledWith("scm_provider");
  });

  it('counts the essentials phase honestly now that it carries the corporate steps', () => {
    const rail = renderRail("environment");
    // Essentials = environment + host_proxy + artifact_repo + provider. The
    // fixture has environment + provider done, the two corporate steps not.
    expect(rail.getByText("2/4")).toBeInTheDocument();
  });

  it('all-optional phases read "all optional", never a counter that cannot fill', () => {
    const rail = renderRail("environment");
    // Two phases are made only of optional steps and so read "all optional":
    // Demos, and "Your work" (scm/credentials/workspaces — credentials is
    // done-pinned false by the honesty law, so a 0/3 counter there could
    // structurally never reach 3/3). Essentials no longer qualifies: it contains
    // the one hard requirement (environment).
    expect(rail.getAllByText("all optional")).toHaveLength(2);
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

  // A4 — "Skipped" state: the rail dot for a visited-but-unconfigured optional
  // step is a muted "visited" marker, never the green done checkmark. PhaseRail
  // stays pure/prop-shape-unchanged — it derives this straight from the badge
  // the orchestrator already computed (badge.text === "Skipped"), the same
  // signal the chip itself renders.
  it("renders a muted visited dot (not a done checkmark) for a step whose badge reads Skipped", () => {
    const badges = { ...BADGES, credentials: { text: "Skipped", tone: "neutral" } as StepBadge };
    render(<PhaseRail current="environment" badges={badges} done={DONE} onSelect={vi.fn()} />);
    const nav = within(screen.getByRole("navigation", { name: /setup steps/i }));
    const btn = nav.getByRole("button", { name: /credentials/i });
    expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    const dot = btn.querySelector("[data-visited]");
    expect(dot).toHaveAttribute("data-visited", "true");
  });

  it("a done step never carries the visited marker, even if its badge text were Skipped", () => {
    // Belt-and-suspenders on the "done always wins" rule: force the contradiction
    // (shouldn't occur in practice — stepDone/the orchestrator never pair
    // done:true with badge text "Skipped" outside the explicit model-skip case,
    // which itself renders the green checkmark) and confirm isDone still wins.
    const badges = { ...BADGES, credentials: { text: "Skipped", tone: "neutral" } as StepBadge };
    const done = { ...DONE, credentials: true };
    render(<PhaseRail current="environment" badges={badges} done={done} onSelect={vi.fn()} />);
    const nav = within(screen.getByRole("navigation", { name: /setup steps/i }));
    const btn = nav.getByRole("button", { name: /credentials/i });
    expect(btn.querySelector("[data-visited]")).toBeNull();
  });
});
