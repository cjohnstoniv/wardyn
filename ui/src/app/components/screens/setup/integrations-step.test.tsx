/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is a thin wrapper: the lede, the embedded list (its own
// coverage lives in integrations/integrations-screen.test.tsx — mocked out
// here so this suite stays scoped to what THIS component owns), the "Manage
// in Integrations" link, and the count/skipped-gated skip control.
import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { IntegrationsStep } from "./integrations-step";
import { T } from "../../../lib/integrations";

vi.mock("../integrations/integrations-screen", () => ({
  IntegrationsScreen: () => <div data-testid="embedded-list" />,
}));

function renderStep(props: Partial<ComponentProps<typeof IntegrationsStep>> = {}) {
  return render(
    <MemoryRouter>
      <IntegrationsStep count={0} skipped={false} onSkip={vi.fn()} onRecheck={vi.fn()} {...props} />
    </MemoryRouter>,
  );
}

describe("IntegrationsStep", () => {
  it("renders the step lede verbatim and embeds the real list, not a second copy", () => {
    renderStep();
    expect(screen.getByText(T.STEP_LEDE)).toBeInTheDocument();
    expect(screen.getByTestId("embedded-list")).toBeInTheDocument();
  });

  it("links to /integrations for deeper management", () => {
    renderStep();
    const link = screen.getByRole("link", { name: /manage in integrations/i });
    expect(link).toHaveAttribute("href", "/integrations");
  });

  it("offers 'Skip this step' with nothing connected and not yet skipped", () => {
    renderStep({ count: 0, skipped: false });
    expect(screen.getByRole("button", { name: /^skip this step$/i })).toBeInTheDocument();
  });

  it("hides the skip control once something is connected — the step already reads Ready", () => {
    renderStep({ count: 2, skipped: false });
    expect(screen.queryByRole("button", { name: /^skip this step$/i })).not.toBeInTheDocument();
  });

  it("hides the skip control once already explicitly skipped", () => {
    renderStep({ count: 0, skipped: true });
    expect(screen.queryByRole("button", { name: /^skip this step$/i })).not.toBeInTheDocument();
  });

  it("clicking Skip calls onSkip", async () => {
    const user = userEvent.setup();
    const onSkip = vi.fn();
    renderStep({ onSkip });
    await user.click(screen.getByRole("button", { name: /^skip this step$/i }));
    expect(onSkip).toHaveBeenCalledTimes(1);
  });
});
