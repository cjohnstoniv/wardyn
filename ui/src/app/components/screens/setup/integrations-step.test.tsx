/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is now a thin composition of the SHARED ModelProviderCard
// (../settings/connection-cards) plus its lede — it no longer embeds the whole
// /integrations page, which is what let an operator-extensibility framework
// (seven kinds, a probe system, an adopt lifecycle) surface during first-run
// setup. GitHostCard retired in 0.7.2; its git credential lanes moved to the
// `providers` step. The card's own behaviour is covered in
// connection-cards.test.tsx; what THIS suite owns is that the step renders it,
// wired to the step's status and recheck.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IntegrationsStep, STEP_LEDE } from "./integrations-step";
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
  it("renders the step lede verbatim", () => {
    renderStep();
    expect(screen.getByText(STEP_LEDE)).toBeInTheDocument();
  });

  // GitHostCard retired in 0.7.2 — the step renders ModelProviderCard only now;
  // the git credential lanes moved to the `providers` step / /providers (its
  // own test coverage).
  it("renders the shared model-provider card", () => {
    renderStep();
    expect(screen.getByRole("radiogroup", { name: S.MODEL_TITLE })).toBeInTheDocument();
  });

  // The regression this whole rework exists to prevent: the step used to embed
  // IntegrationsScreen, so the funnel showed a catalog with an "Add integration"
  // button during first-run setup.
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
    // Azure was the fifth AI kind; it only ever powered the deleted composer.
    expect(screen.queryByText(/Azure/i)).not.toBeInTheDocument();
  });

  it("offers no git-host lanes — retired with GitHostCard", () => {
    renderStep();
    expect(screen.queryByRole("radio", { name: /Personal access token/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /GitHub App/ })).not.toBeInTheDocument();
  });
});
