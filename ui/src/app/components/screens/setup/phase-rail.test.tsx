/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PhaseRail } from "./phase-rail";
import { STEP_LABEL, type SetupStepId, type StepBadge } from "./steps";
import { DEMOS } from "../demos/demo-catalog";

// Exhaustive over SetupStepId (the compiler enforces it) — every demo
// sub-step included, since Getting Started absorbed the whole catalog.
const BADGES: Record<SetupStepId, StepBadge> = {
  environment: { text: "Ready · 2 of 3 barriers", tone: "success" },
  people: { text: "Single-user", tone: "neutral" },
  corp_network: { text: "Optional", tone: "neutral" },
  integrations: { text: "Ready · 2 connected", tone: "success" },
  "sealed-box": { text: "Optional", tone: "neutral" },
  "fail-then-approve": { text: "Optional", tone: "neutral" },
  "held-at-the-door": { text: "Optional", tone: "neutral" },
  "lines-that-cant-be-crossed": { text: "Optional", tone: "neutral" },
  "denied-however-spelled": { text: "Optional", tone: "neutral" },
  "agent-in-the-box": { text: "Optional", tone: "neutral" },
  "record-a-policy": { text: "Optional", tone: "neutral" },
  "once-or-for-good": { text: "Optional", tone: "neutral" },
  "write-only-by-design": { text: "Optional", tone: "neutral" },
  "key-never-in-the-box": { text: "Optional", tone: "neutral" },
  "authorized-not-issued": { text: "Optional", tone: "neutral" },
  "rest-api-token": { text: "Optional", tone: "neutral" },
  "pat-stdout-only": { text: "Optional", tone: "neutral" },
  "ssh-briefly-resident": { text: "Optional", tone: "neutral" },
  "github-app-broker": { text: "Optional", tone: "neutral" },
  "sts-fail-closed": { text: "Optional", tone: "neutral" },
  providers: { text: "Optional", tone: "neutral" },
  workspaces: { text: "In progress", tone: "info" },
  review: { text: "Review what's left", tone: "neutral" },
};

const DONE: Record<SetupStepId, boolean> = {
  environment: true,
  people: true,
  corp_network: false,
  integrations: true,
  "sealed-box": false,
  "fail-then-approve": false,
  "held-at-the-door": false,
  "lines-that-cant-be-crossed": false,
  "denied-however-spelled": false,
  "agent-in-the-box": false,
  "record-a-policy": false,
  "once-or-for-good": false,
  "write-only-by-design": false,
  "key-never-in-the-box": false,
  "authorized-not-issued": false,
  "rest-api-token": false,
  "pat-stdout-only": false,
  "ssh-briefly-resident": false,
  "github-app-broker": false,
  "sts-fail-closed": false,
  providers: false,
  workspaces: false,
  review: false,
};

// PhaseRail is a pure, generic component: it renders whatever `order` names,
// and its own default (steps.ts's STEP_ORDER) is the REAL admin-funnel walk,
// which M-6 (D5) trimmed to 7 ids once the demos left it. These tests still
// want the rich, ~23-id exhaustive fixture BADGES/DONE already are — that's
// what lets "renders every step label" and the group-count test mean
// anything — so they pass it explicitly rather than leaning on the shrunk
// production default. Derived from BADGES (itself exhaustive over
// SetupStepId, the compiler enforces it), not hand-kept, for the same reason
// BADGES/DONE are keyed that way.
const FULL_ORDER = Object.keys(BADGES) as SetupStepId[];

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
  render(<PhaseRail current={current} badges={BADGES} done={DONE} onSelect={onSelect} order={FULL_ORDER} />);
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

  it("renders every step label as a button in the full rail", () => {
    const rail = renderRail("environment");
    for (const label of Object.values(STEP_LABEL)) {
      // Titles contain regex metacharacters ("Lines that can't be crossed"),
      // and one demo title is a prefix of nothing else — escape, don't anchor.
      expect(
        rail.getByRole("button", { name: new RegExp(label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i") }),
      ).toBeInTheDocument();
    }
  });

  // The rail offers only what stepOrder(status) says is walkable: a demo whose
  // needsModel/needsSecret precondition is unmet is not a step you can open,
  // and a demo SUBSECTION left with no surviving steps renders nothing —
  // never an empty "Secrets" heading over a 0.
  it("omits filtered-out steps, and drops a demo subsection left with none of them", () => {
    cleanup();
    // Derived from the catalog, not a hand-kept id list: the point is a
    // subsection with NO surviving steps, and a literal list silently stops
    // emptying the Secrets subsection the moment a demo is added to it.
    const dropped = new Set<string>(["agent-in-the-box", ...DEMOS.filter((d) => d.section === "secrets").map((d) => d.id)]);
    const order = FULL_ORDER.filter((id) => !dropped.has(id));
    render(<PhaseRail current="environment" badges={BADGES} done={DONE} onSelect={vi.fn()} order={order} />);
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const rail = within(navs[navs.length - 1]);
    // The demo subsection headings render as <div>s; the config step's own
    // rail-item label ("Secrets", for `integrations`) is a <span> and is
    // ALWAYS present — so the subsection query is scoped to div to avoid a
    // false multi-match against that unrelated item.
    expect(rail.getByText("Egress", { selector: "div" })).toBeInTheDocument();
    expect(rail.queryByText("Secrets", { selector: "div" })).not.toBeInTheDocument();
    expect(rail.queryByRole("button", { name: /the agent in the box/i })).toBeNull();
    expect(rail.getByRole("button", { name: /record a policy/i })).toBeInTheDocument();
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

  // F3-F3: the rail is the SAME crossing predicate the footer's Next button
  // already renders disabled+titled — without this, a rail click past an
  // ungated corp_network read as a live, clickable step whose onSelect just
  // silently no-oped (a dead click, not a disabled one). Wired identically on
  // both rails (compact + full); this exercises the full one.
  it("a step refused by refuseNext renders disabled with the refusal as its title, on both rails", () => {
    // ticket: F3-F3
    cleanup();
    const refuseNext = (next: SetupStepId) =>
      next === "workspaces" || next === "review" ? "Prove network access first." : undefined;
    render(
      <PhaseRail
        current="environment"
        badges={BADGES}
        done={DONE}
        onSelect={vi.fn()}
        refuseNext={refuseNext}
        order={FULL_ORDER}
      />,
    );
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const fullRail = within(navs[navs.length - 1]);
    const compactRail = within(navs[0]);
    for (const rail of [fullRail, compactRail]) {
      const workspaces = rail.getByRole("button", { name: /workspaces/i });
      expect(workspaces).toBeDisabled();
      expect(workspaces).toHaveAttribute("title", "Prove network access first.");
    }
    // Negative control: an earlier step (environment, at-or-before current)
    // and a demo step (never gated by the crossing predicate) both stay live.
    for (const rail of [fullRail, compactRail]) {
      expect(rail.getByRole("button", { name: /^environment/i })).not.toBeDisabled();
      expect(rail.getByRole("button", { name: /record a policy/i })).not.toBeDisabled();
    }
  });

  it("negative control: with no refuseNext, no button in either rail is ever disabled", () => {
    // ticket: F3-F3
    renderRail("environment");
    for (const btn of screen.getAllByRole("button")) {
      expect(btn).not.toBeDisabled();
    }
  });

  // #213 — three categories, not the old five phases. Each group header now
  // shows a plain count (not an X/Y progress fraction — Optional setup and
  // Demos have no notion of "done" any more; per-step dots still track real
  // progress inside each group).
  it("shows the three group headers with their counts (Required · 4, Optional setup · 3, Demos · N)", () => {
    // renderRail's default `order` is FULL_ORDER (the full 16-demo catalog,
    // not a status-walked subset, and not the shrunk 7-id production
    // default since M-6/D5) — see the demo-catalog count pin below for why
    // 16, not 10.
    const rail = renderRail("environment");
    expect(rail.getByText("Required")).toBeInTheDocument();
    expect(rail.getByText("· 4")).toBeInTheDocument();
    expect(rail.getByText("Optional setup")).toBeInTheDocument();
    expect(rail.getByText("· 3")).toBeInTheDocument();
    expect(rail.getByText("Demos")).toBeInTheDocument();
    expect(rail.getByText(`· ${DEMOS.length}`)).toBeInTheDocument();
  });

  it("numbers only the Required group's items 1-4 (decorative, aria-hidden — never in the button's accessible name)", () => {
    const rail = renderRail("environment");
    const nums = rail.getAllByText(/^[1-4]$/, { selector: "span[aria-hidden]" }).map((el) => el.textContent);
    expect(nums).toEqual(["1", "2", "3", "4"]);
    // The Optional-setup/Demos items carry no ordinal at all.
    const secrets = rail.getByRole("button", { name: /^secrets/i });
    expect(within(secrets).queryByText(/^[0-9]+$/)).not.toBeInTheDocument();
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
