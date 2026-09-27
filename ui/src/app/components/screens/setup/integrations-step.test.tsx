/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is a thin composition of the shared ModelProviderCard
// (../settings/connection-cards) plus its lede — it does not embed the whole
// /integrations page, which is what would let an operator-extensibility
// framework (seven kinds, a probe system, an adopt lifecycle) surface during
// first-run setup. Git credential lanes live in the `providers` step, not
// here. The card's own behaviour is covered in connection-cards.test.tsx;
// what this suite owns is that the step renders it, wired to the step's
// status and recheck.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IntegrationsStep, STEP_LEDE_LINK, STEP_LEDE_PREFIX, STEP_LEDE_SUFFIX } from "./integrations-step";
import { S } from "../settings/connection-cards";
import { baseStatus } from "../../../lib/test-fixtures";

function renderStep() {
  return render(
    <MemoryRouter>
      <IntegrationsStep status={baseStatus()} siteConfig={null} onRecheck={vi.fn()} />
    </MemoryRouter>,
  );
}

describe("IntegrationsStep", () => {
  it("renders the step lede verbatim, around a real link to /secrets", () => {
    renderStep();
    // Split by the link (react-router-dom's <Link>), so the surrounding text
    // is asserted with a function matcher rather than a single exact node.
    expect(
      screen.getByText((_, node) => node?.textContent === STEP_LEDE_PREFIX + STEP_LEDE_LINK + STEP_LEDE_SUFFIX),
    ).toBeInTheDocument();
  });

  // X3-F8: the lede's "Secrets page" mention must be a real link, not just
  // named text with no way to get there.
  it("the lede's Secrets page mention is a real link to /secrets", () => {
    // ticket: X3-F8
    renderStep();
    const link = screen.getByRole("link", { name: STEP_LEDE_LINK });
    expect(link).toHaveAttribute("href", "/secrets");
  });

  // The step renders ModelProviderCard only — git credential lanes live in
  // the `providers` step / /providers (its own test coverage), not here.
  it("renders the shared model-provider card", () => {
    renderStep();
    expect(screen.getByRole("radiogroup", { name: S.MODEL_TITLE })).toBeInTheDocument();
  });

  // The regression this whole rework exists to prevent: embedding
  // IntegrationsScreen would show the funnel a catalog with an "Add
  // integration" button during first-run setup.
  it("offers no integration catalog — no Add integration affordance anywhere", () => {
    renderStep();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/No integrations/i)).not.toBeInTheDocument();
  });

  it("names the three model lanes the mock settled on", () => {
    renderStep();
    expect(screen.getByRole("radio", { name: /Claude subscription/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /API key/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /AWS Bedrock/ })).toBeInTheDocument();
    // Azure is one of the five AI kinds the API can return, but only the
    // deleted composer ever used it, so it must never render here.
    expect(screen.queryByText(/Azure/i)).not.toBeInTheDocument();
  });

  it("offers no git-host lanes — retired with GitHostCard", () => {
    renderStep();
    expect(screen.queryByRole("radio", { name: /Personal access token/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /GitHub App/ })).not.toBeInTheDocument();
  });
});
