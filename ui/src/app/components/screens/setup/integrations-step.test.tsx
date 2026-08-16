/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is now a thin composition of the two SHARED connection cards
// (../settings/connection-cards) plus its lede — it no longer embeds the whole
// /integrations page, which is what let an operator-extensibility framework
// (seven kinds, a probe system, an adopt lifecycle) surface during first-run
// setup. The cards' own behaviour is covered in connection-cards.test.tsx; what
// THIS suite owns is that the step renders both of them, in order, wired to the
// step's status and recheck.
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

  it("renders BOTH shared connection cards — model provider first, git host second", () => {
    renderStep();
    const model = screen.getByRole("radiogroup", { name: S.MODEL_TITLE });
    const git = screen.getByRole("radiogroup", { name: S.GIT_TITLE });
    expect(model).toBeInTheDocument();
    expect(git).toBeInTheDocument();
    // A model is what gates a first agent run; the git credential only matters
    // once there's a private repo. Order is the teaching, so it is pinned.
    expect(model.compareDocumentPosition(git) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
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

  it("names the three git-host lanes", () => {
    renderStep();
    expect(screen.getByRole("radio", { name: /Personal access token/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /SSH key/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /GitHub App/ })).toBeInTheDocument();
  });
});
