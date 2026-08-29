/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SetupLayout } from "./setup-layout";
import { OPTIONAL_STEPS, STEP_ORDER, type SetupStepId } from "./steps";

function renderLayout(overrides: Partial<ComponentProps<typeof SetupLayout>> = {}) {
  const props: ComponentProps<typeof SetupLayout> = {
    current: "environment",
    rail: <div>rail</div>,
    checking: false,
    lastCheckedLabel: "just now",
    onRecheck: vi.fn(),
    onSelect: vi.fn(),
    onFinish: vi.fn(),
    operator: true,
    children: <div>step body</div>,
    ...overrides,
  };
  return render(<SetupLayout {...props} />);
}

describe("SetupLayout", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  it("renders the step heading from STEP_HEADING and an Optional chip on an optional step", () => {
    renderLayout({ current: "integrations" });
    expect(screen.getByRole("heading", { name: /^secrets$/i })).toBeInTheDocument();
    expect(screen.getByText("Optional")).toBeInTheDocument();
  });

  it("does not render an Optional chip on the environment step", () => {
    renderLayout({ current: "environment" });
    expect(screen.getByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.queryByText("Optional")).not.toBeInTheDocument();
  });

  // People directly follows Environment (Phase 5) — see steps.ts's PHASES.
  it("footer renders 'Next: People' on the environment step and calls onSelect(\"people\")", async () => {
    const onSelect = vi.fn();
    renderLayout({ current: "environment", onSelect });
    const nextBtn = screen.getByRole("button", { name: /^next: people$/i });
    await user.click(nextBtn);
    expect(onSelect).toHaveBeenCalledWith("people");
  });

  // No phase is collapsible in the rail, so the phase-level skip control never
  // renders — every optional step (including Integrations) is simply one you
  // Next past; there is no separate in-step skip control any more (dfb51d8
  // deleted the last of them).
  it("offers no phase-level skip control now that no phase is collapsible", () => {
    renderLayout({ current: "integrations" });
    expect(screen.queryByRole("button", { name: /skip network/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^skip .* network$/i })).not.toBeInTheDocument();
  });

  // Generic gate — SetupLayout has no idea which step set this or why (see
  // steps.ts's corpNetworkGate, the current sole producer of one).
  describe("nextGate — the footer's generic Next gate (head/reason/action)", () => {
    it("Next is enabled and nothing extra renders when nextGate is absent", () => {
      renderLayout({ current: "environment" });
      expect(screen.getByRole("button", { name: /^next: people$/i })).toBeEnabled();
    });

    it("blocked without an action: Next is disabled, the head and reason render", () => {
      renderLayout({
        current: "environment",
        nextGate: { blocked: true, head: "Connectivity isn't proven yet", reason: "One probe, and this step is done.", tone: "warning" },
      });
      expect(screen.getByRole("button", { name: /^next: people$/i })).toBeDisabled();
      expect(screen.getByText("Connectivity isn't proven yet")).toBeInTheDocument();
      expect(screen.getByText("One probe, and this step is done.")).toBeInTheDocument();
    });

    it("blocked WITH an action: the action button renders IN PLACE of Next and fires its handler", async () => {
      const onAction = vi.fn();
      const onSelect = vi.fn();
      renderLayout({
        current: "environment",
        onSelect,
        nextGate: {
          blocked: true,
          head: "Connectivity isn't proven yet",
          reason: "One probe.",
          action: { label: "Test connectivity", onClick: onAction },
        },
      });
      expect(screen.queryByRole("button", { name: /^next: people$/i })).not.toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^test connectivity$/i }));
      expect(onAction).toHaveBeenCalled();
      expect(onSelect).not.toHaveBeenCalled();
    });

    // W13-S1-8: the action button stands in for corp-network-egress.tsx's
    // inline Test/Test-all buttons, which are already disabled={!operator} —
    // this shared footer button had no such check.
    it("blocked WITH an action, a non-operator: the button is disabled and never fires", async () => {
      const onAction = vi.fn();
      renderLayout({
        current: "environment",
        operator: false,
        nextGate: {
          blocked: true,
          head: "Connectivity isn't proven yet",
          reason: "One probe.",
          action: { label: "Test connectivity", onClick: onAction },
        },
      });
      const button = screen.getByRole("button", { name: /^test connectivity$/i });
      expect(button).toBeDisabled();
      await user.click(button);
      expect(onAction).not.toHaveBeenCalled();
    });

    it("an ENABLED gate with a head/reason renders them as a neutral standing note beside a working Next", async () => {
      const onSelect = vi.fn();
      renderLayout({
        current: "environment",
        onSelect,
        nextGate: { blocked: false, head: "Nothing to test with", reason: "Nothing was proven here.", tone: "neutral" },
      });
      const next = screen.getByRole("button", { name: /^next: people$/i });
      expect(next).toBeEnabled();
      expect(screen.getByText("Nothing to test with")).toBeInTheDocument();
      expect(screen.getByText("Nothing was proven here.")).toBeInTheDocument();
      await user.click(next);
      expect(onSelect).toHaveBeenCalled();
    });

    it("an enabled gate's action is IGNORED — the fix-it button exists only while blocked", () => {
      renderLayout({
        current: "environment",
        nextGate: { blocked: false, action: { label: "Should not render", onClick: vi.fn() } },
      });
      expect(screen.getByRole("button", { name: /^next: people$/i })).toBeEnabled();
      expect(screen.queryByRole("button", { name: /should not render/i })).not.toBeInTheDocument();
    });

    it("nextLabel/onNext rename and repoint a rendered Next — the step's own internal stop, not the exit", async () => {
      const onNext = vi.fn();
      const onSelect = vi.fn();
      renderLayout({
        current: "corp_network",
        onSelect,
        nextGate: { blocked: false, nextLabel: "Next: Egress redirection", onNext },
      });
      const btn = screen.getByRole("button", { name: /^next: egress redirection$/i });
      await user.click(btn);
      expect(onNext).toHaveBeenCalled();
      expect(onSelect).not.toHaveBeenCalled();
    });

    it("the relabel applies to a DISABLED Next too (probe in flight already names where a pass will go)", () => {
      renderLayout({
        current: "corp_network",
        nextGate: { blocked: true, head: "Probe in flight", tone: "neutral", nextLabel: "Next: Egress redirection" },
      });
      expect(screen.getByRole("button", { name: /^next: egress redirection$/i })).toBeDisabled();
    });

    it("backOverride repoints Back (the mirror: Egress redirection returns to Host proxy)", async () => {
      const backOverride = vi.fn();
      const onSelect = vi.fn();
      renderLayout({ current: "corp_network", onSelect, backOverride });
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      expect(backOverride).toHaveBeenCalled();
      expect(onSelect).not.toHaveBeenCalled();
    });

    it("the gate never renders on the LAST step (there is no Next to gate)", () => {
      renderLayout({
        current: "review",
        nextGate: { blocked: true, head: "Should never appear here.", reason: "Nor this." },
      });
      expect(screen.queryByText("Should never appear here.")).not.toBeInTheDocument();
    });
  });

  // #11: the footer used to render its OWN "Launch your first run" button
  // (gated on canLaunch) — an exact duplicate of step-bodies.tsx's LaunchStep,
  // which renders inside `children` on the last step and owns that gating
  // now (see step-bodies.test.tsx). Deleted here; nothing left to test at
  // this layer once the shell stopped taking onLaunch/canLaunch at all.
  it("renders no launch button of its own on the last step — only 'Finish setup' and 'Back'", () => {
    renderLayout({ current: "review" });
    expect(screen.queryByRole("button", { name: /launch your first run/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /finish setup/i })).toBeInTheDocument();
  });

  // The fast-path banner ("You're ready — launch your first run now" + Keep
  // setting up) was REMOVED: it duplicated the Launch step's own affordance and
  // shouted over the step the operator was actually working on. Launching early
  // is still available from the rail's Launch step at any time.
  it("renders no fast-path banner even when fully ready", () => {
    renderLayout({ current: "workspaces" });
    expect(screen.queryByText(/you're ready — launch your first run now/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /keep setting up/i })).not.toBeInTheDocument();
  });
});

// MEDIUM-2: nextGate is produced only ON corp_network, so every step AFTER it
// that the orchestrator's crossing guard still refuses used to render a live
// Next whose click silently no-op'd. The shell asks the same predicate the
// guard uses and disables the button, with the gate's reason as its title.
describe("refuseNext — a refused forward move renders a DISABLED Next, not a dead-enabled one", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  it("disables Next and titles it with the refusal reason", async () => {
    const onSelect = vi.fn();
    renderLayout({
      current: "sealed-box",
      onSelect,
      refuseNext: () => "Connectivity isn't proven yet.",
    });
    const next = screen.getByRole("button", { name: /^next:/i });
    expect(next).toBeDisabled();
    expect(next).toHaveAttribute("title", "Connectivity isn't proven yet.");
    await user.click(next);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("undefined means allowed — Next stays enabled and untitled", () => {
    renderLayout({ current: "sealed-box", refuseNext: () => undefined });
    const next = screen.getByRole("button", { name: /^next:/i });
    expect(next).toBeEnabled();
    expect(next).not.toHaveAttribute("title");
  });
});

// M3: the shell walks stepOrder(status), not the full contract — a filtered-out
// conditional demo is neither counted in "Step N of M" nor stepped through.
describe("order — the walked steps, not the whole contract", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  it("counts and steps through only the given order", async () => {
    const onSelect = vi.fn();
    const order: SetupStepId[] = ["environment", "corp_network", "integrations", "sealed-box", "review"];
    renderLayout({ current: "sealed-box", order, onSelect });
    expect(screen.getByText("Step 4 of 5")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^next: review$/i }));
    expect(onSelect).toHaveBeenCalledWith("review");
    await user.click(screen.getByRole("button", { name: /^back$/i }));
    expect(onSelect).toHaveBeenCalledWith("integrations");
  });
});

describe("Optional chip — set-exact across every step", () => {
  for (const step of STEP_ORDER) {
    it(`${OPTIONAL_STEPS.has(step) ? "shows" : "hides"} the Optional chip on ${step}`, () => {
      renderLayout({ current: step });
      const heading = screen.getByRole("heading", { level: 2 });
      const row = heading.parentElement?.textContent ?? "";
      if (OPTIONAL_STEPS.has(step)) {
        expect(row).toMatch(/Optional/);
      } else {
        expect(row).not.toMatch(/Optional/);
      }
      cleanup();
    });
  }
});
