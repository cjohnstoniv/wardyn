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
    onFinishLater: vi.fn(),
    onLaunch: vi.fn(),
    canLaunch: true,
    fastPath: false,
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

  it("footer renders 'Next: Model/Harness Provider' on the environment step and calls onSelect(\"provider\")", async () => {
    const onSelect = vi.fn();
    renderLayout({ current: "environment", onSelect });
    const nextBtn = screen.getByRole("button", { name: /^next: model\/harness provider$/i });
    await user.click(nextBtn);
    expect(onSelect).toHaveBeenCalledWith("provider");
  });

  it("last step renders the Launch button disabled when canLaunch is false", () => {
    renderLayout({ current: "launch", canLaunch: false });
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeDisabled();
  });

  it("last step renders the Launch button enabled when canLaunch is true", () => {
    renderLayout({ current: "launch", canLaunch: true });
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeEnabled();
  });

  it("fast-path banner is absent when fastPath is false", () => {
    renderLayout({ fastPath: false });
    expect(screen.queryByText(/you're ready — launch your first run now/i)).not.toBeInTheDocument();
  });

  it("fast-path banner renders only when fastPath is true, with the connected model label", () => {
    // connectedModelLabel carries the full deriveReadiness().llmLabel fragment.
    renderLayout({ fastPath: true, connectedModelLabel: "Claude connected" });
    expect(screen.getByText(/you're ready — launch your first run now/i)).toBeInTheDocument();
    expect(screen.getByText(/a barrier is up and claude connected/i)).toBeInTheDocument();
  });
});

describe("Optional chip — set-exact across all nine steps", () => {
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
