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

const integrationsScreenPropsSpy = vi.fn();
vi.mock("../integrations/integrations-screen", () => ({
  IntegrationsScreen: (props: unknown) => {
    integrationsScreenPropsSpy(props);
    return <div data-testid="embedded-list" />;
  },
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

  // Corporate-network restructure: host proxy + egress redirection moved out
  // of this embed and into their own step — the embedded list is told to hide
  // both categories, and the note explaining where they went always renders
  // (not just once something's connected — a first, empty visit is exactly
  // when someone wonders where those two categories are).
  it("hides the host-proxy and egress-redirection categories from the embed and explains why", () => {
    renderStep();
    expect(integrationsScreenPropsSpy).toHaveBeenCalledWith(
      expect.objectContaining({ embedded: true, hideCategories: ["host_proxy", "artifact_mirror"] }),
    );
    expect(screen.getByText(T.EMBED_SCOPE_NOTE)).toBeInTheDocument();
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
