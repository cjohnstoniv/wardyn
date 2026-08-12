/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is a thin wrapper: the lede, the embedded list (its own
// coverage lives in integrations/integrations-screen.test.tsx — mocked out
// here so this suite stays scoped to what THIS component owns), and the
// scope note. It has NO footer of its own any more: "Manage in Integrations"
// duplicated the embed (this IS that page), and "Skip this step" duplicated
// Next — skipping is what clicking Next past the step means now
// (setup-screen.test.tsx covers that orchestrator rule).
import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
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
      <IntegrationsStep onRecheck={vi.fn()} {...props} />
    </MemoryRouter>,
  );
}

describe("IntegrationsStep", () => {
  it("renders the step lede verbatim and embeds the real list, not a second copy", () => {
    renderStep();
    expect(screen.getByText(T.STEP_LEDE)).toBeInTheDocument();
    expect(screen.getByTestId("embedded-list")).toBeInTheDocument();
  });

  // Corporate-network consolidation: host proxy + egress redirection are gone
  // from the Integrations page itself, so there's nothing for this embed to
  // hide — it passes no hideCategories at all. The note explaining where they
  // went always renders (not just once something's connected — a first, empty
  // visit is exactly when someone wonders where those two categories are).
  it("passes no hideCategories — the two categories are gone from the page, not hidden from the embed", () => {
    renderStep();
    expect(integrationsScreenPropsSpy).toHaveBeenCalledWith(expect.objectContaining({ embedded: true }));
    expect(integrationsScreenPropsSpy.mock.calls[0][0]).not.toHaveProperty("hideCategories");
    expect(screen.getByText(T.EMBED_SCOPE_NOTE)).toBeInTheDocument();
  });

  // The two dead affordances, pinned dead: the manage link pointed at the
  // page this step already embeds, and the skip button duplicated Next.
  it("renders neither a 'Manage in Integrations' link nor a 'Skip this step' button", () => {
    renderStep();
    expect(screen.queryByRole("link", { name: /manage in integrations/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^skip this step$/i })).not.toBeInTheDocument();
  });
});
