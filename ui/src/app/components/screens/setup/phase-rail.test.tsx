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
  corp_network: { text: "Optional", tone: "neutral" },
  integrations: { text: "Ready · 2 connected", tone: "success" },
  "sealed-box": { text: "Optional", tone: "neutral" },
  "fail-then-approve": { text: "Optional", tone: "neutral" },
  "held-at-the-door": { text: "Optional", tone: "neutral" },
  "lines-that-cant-be-crossed": { text: "Optional", tone: "neutral" },
  "once-or-for-good": { text: "Optional", tone: "neutral" },
  workspaces: { text: "In progress", tone: "info" },
  review: { text: "Review what's left", tone: "neutral" },
};

const DONE: Record<SetupStepId, boolean> = {
  environment: true,
  corp_network: false,
  integrations: true,
  "sealed-box": false,
  "fail-then-approve": false,
  "held-at-the-door": false,
  "lines-that-cant-be-crossed": false,
  "once-or-for-good": false,
  workspaces: false,
  review: false,
};

// The compact icon rail (lg-only) renders every step unconditionally (CSS-hidden,
// not DOM-absent — jsdom doesn't apply Tailwind's responsive `hidden`), so its
// buttons share accessible names with the full rail's. Scope queries to the full
// rail's nav landmark, same as production CSS would at xl+. Both rails carry the
// "Setup steps" nav landmark (ui-setup-5) and so share an accessible name — CSS
// makes only one visible at a time in a real browser; jsdom renders both, so pick
// the full rail explicitly: it's the SECOND "Setup steps" nav in DOM order (the
// compact rail is declared first in PhaseRail's JSX).
function renderRail(current: SetupStepId, onSelect = vi.fn()) {
  cleanup(); // some tests render twice (collapsed vs. expanded) to compare
  render(<PhaseRail current={current} badges={BADGES} done={DONE} onSelect={onSelect} />);
  const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
  return within(navs[navs.length - 1]);
}

describe("PhaseRail", () => {
  // ui-setup-5: only the full rail carried the nav/aria-label landmark; the
  // compact (lg-only) icon rail was a bare div, invisible to landmark
  // navigation for a screen-reader operator on that breakpoint.
  it("ui-setup-5: the compact icon rail also carries its own Setup steps nav landmark", () => {
    cleanup();
    render(<PhaseRail current="environment" badges={BADGES} done={DONE} onSelect={vi.fn()} />);
    expect(screen.getAllByRole("navigation", { name: /setup steps/i })).toHaveLength(2);
  });

  it("a full-rail step button carries both the frozen label and its badge text", () => {
    const rail = renderRail("environment");
    const btn = rail.getByRole("button", { name: /^secrets/i });
    expect(within(btn).getByText("Ready · 2 connected")).toBeInTheDocument();
  });

  it("renders all 10 frozen labels as buttons in the full rail", () => {
    const rail = renderRail("environment");
    for (const label of Object.values(STEP_LABEL)) {
      expect(rail.getByRole("button", { name: new RegExp(label, "i") })).toBeInTheDocument();
    }
  });

  // The 13->9 collapse folded the old corporate-network steps (Host Proxy / SCM
  // Provider / Artifact Redirect) and the model picker into ONE Integrations
  // step, right after Environment in Essentials — no expander, never collapsed.
  it("shows the connection step inline in Essentials, right after Environment, with no expander to open", () => {
    const rail = renderRail("environment");
    expect(rail.getByRole("button", { name: /^secrets/i })).toBeInTheDocument();
    expect(rail.queryByRole("button", { name: /^essentials$/i })).not.toBeInTheDocument();
  });

  it("clicking a step button calls onSelect with its id", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const rail = renderRail("environment", onSelect);
    await user.click(rail.getByRole("button", { name: /^secrets/i }));
    expect(onSelect).toHaveBeenCalledWith("integrations");
  });

  it("counts the essentials phase honestly (environment + corp_network + integrations)", () => {
    const rail = renderRail("environment");
    // Essentials = environment + corp_network + integrations. The fixture has
    // environment and integrations done, corp_network merely Skipped (not
    // done) — 2 of 3, an honest partial count.
    expect(rail.getByText("2/3")).toBeInTheDocument();
  });

  it('all-optional phases read "all optional", never a counter that cannot fill', () => {
    const rail = renderRail("environment");
    // Two phases are made only of optional steps and so read "all optional":
    // Demos, and "Your work" (now just workspaces). Essentials no longer
    // qualifies: it contains the one hard requirement (environment).
    expect(rail.getAllByText("all optional")).toHaveLength(2);
    // Finish is one step now (Review) — the Launch step was cut, so its counter
    // is 0/1, not 0/2.
    expect(rail.getByText("0/1")).toBeInTheDocument();
  });

  it("marks only the active step aria-current=step, and no button anywhere uses aria-pressed", () => {
    const rail = renderRail("integrations");
    expect(rail.getByRole("button", { name: /^secrets/i })).toHaveAttribute("aria-current", "step");
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
    const badges = { ...BADGES, workspaces: { text: "Skipped", tone: "neutral" } as StepBadge };
    render(<PhaseRail current="environment" badges={badges} done={DONE} onSelect={vi.fn()} />);
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = within(navs[navs.length - 1]);
    const btn = nav.getByRole("button", { name: /workspaces/i });
    expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    const dot = btn.querySelector("[data-visited]");
    expect(dot).toHaveAttribute("data-visited", "true");
  });

  it("a done step never carries the visited marker, even if its badge text were Skipped", () => {
    // Belt-and-suspenders on the "done always wins" rule: force the contradiction
    // (shouldn't occur in practice) and confirm isDone still wins.
    const badges = { ...BADGES, workspaces: { text: "Skipped", tone: "neutral" } as StepBadge };
    const done = { ...DONE, workspaces: true };
    render(<PhaseRail current="environment" badges={badges} done={done} onSelect={vi.fn()} />);
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = within(navs[navs.length - 1]);
    const btn = nav.getByRole("button", { name: /workspaces/i });
    expect(btn.querySelector("[data-visited]")).toBeNull();
  });
});
