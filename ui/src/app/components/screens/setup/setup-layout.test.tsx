/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SetupLayout } from "./setup-layout";
import { OPTIONAL_STEPS, STEP_ORDER } from "./steps";

function renderLayout(overrides: Partial<ComponentProps<typeof SetupLayout>> = {}) {
  const props: ComponentProps<typeof SetupLayout> = {
    current: "environment",
    rail: <div>rail</div>,
    checking: false,
    lastCheckedLabel: "just now",
    onRecheck: vi.fn(),
    onSelect: vi.fn(),
    onFinish: vi.fn(),
    onLaunch: vi.fn(),
    canLaunch: true,
    children: <div>step body</div>,
    ...overrides,
  };
  return render(<SetupLayout {...props} />);
}

describe("SetupLayout", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  it("renders the step heading from STEP_HEADING and an Optional chip on an optional step", () => {
    renderLayout({ current: "host_proxy" });
    expect(screen.getByRole("heading", { name: /corporate host proxy/i })).toBeInTheDocument();
    expect(screen.getByText("Optional")).toBeInTheDocument();
  });

  it("does not render an Optional chip on the environment step", () => {
    renderLayout({ current: "environment" });
    expect(screen.getByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.queryByText("Optional")).not.toBeInTheDocument();
  });

  // Host Proxy now directly follows Environment: the corporate-network steps moved
  // into Essentials ahead of the model step, because connecting a model needs egress.
  it("footer renders 'Next: Host Proxy' on the environment step and calls onSelect(\"host_proxy\")", async () => {
    const onSelect = vi.fn();
    renderLayout({ current: "environment", onSelect });
    const nextBtn = screen.getByRole("button", { name: /^next: host proxy$/i });
    await user.click(nextBtn);
    expect(onSelect).toHaveBeenCalledWith("host_proxy");
  });

  // No phase is collapsible any more (the corporate group was the only one), so the
  // phase-level skip control never renders — the corporate steps are simply optional
  // steps you click past, exactly like the model step.
  it("offers no phase-level skip control now that no phase is collapsible", () => {
    renderLayout({ current: "host_proxy" });
    expect(screen.queryByRole("button", { name: /skip corporate network/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^skip .* network$/i })).not.toBeInTheDocument();
  });

  it("last step renders the Launch button disabled when canLaunch is false", () => {
    renderLayout({ current: "launch", canLaunch: false });
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeDisabled();
  });

  it("last step renders the Launch button enabled when canLaunch is true", () => {
    renderLayout({ current: "launch", canLaunch: true });
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeEnabled();
  });

  // The fast-path banner ("You're ready — launch your first run now" + Keep
  // setting up) was REMOVED: it duplicated the Launch step's own affordance and
  // shouted over the step the operator was actually working on. Launching early
  // is still available from the rail's Launch step at any time.
  it("renders no fast-path banner even when fully ready", () => {
    renderLayout({ current: "workspaces", canLaunch: true });
    expect(screen.queryByText(/you're ready — launch your first run now/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /keep setting up/i })).not.toBeInTheDocument();
  });
});

describe("Optional chip — set-exact across all ten steps", () => {
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
