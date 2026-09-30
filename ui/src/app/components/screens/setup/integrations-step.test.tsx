/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// IntegrationsStep is its lede alone — it does not embed the whole
// /integrations page, which is what would let an operator-extensibility
// framework (seven kinds, a probe system, an adopt lifecycle) surface during
// first-run setup. Git credential lanes live in the `providers` step, not
// here.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IntegrationsStep, STEP_LEDE_LINK, STEP_LEDE_PREFIX, STEP_LEDE_SUFFIX } from "./integrations-step";

function renderStep() {
  return render(
    <MemoryRouter>
      <IntegrationsStep />
    </MemoryRouter>,
  );
}

describe("IntegrationsStep", () => {
  it("renders the step lede verbatim, around a real link to /secrets", () => {
    renderStep();
    // Split by the link (react-router-dom's <Link>), so the surrounding text
    // is asserted with a function matcher rather than a single exact node.
    expect(
      screen.getByText(
        (_, node) => node?.tagName === "P" && node.textContent === STEP_LEDE_PREFIX + STEP_LEDE_LINK + STEP_LEDE_SUFFIX,
      ),
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

  // The retired Model provider card (its pointer sentence was never approved
  // canon) must not mount here.
  it("renders no Model provider card", () => {
    renderStep();
    expect(screen.queryByRole("heading", { name: "Model provider" })).not.toBeInTheDocument();
    expect(screen.queryByText(/set up as model providers/i)).not.toBeInTheDocument();
  });

  // The regression this whole rework exists to prevent: embedding
  // IntegrationsScreen would show the funnel a catalog with an "Add
  // integration" button during first-run setup.
  it("offers no integration catalog — no Add integration affordance anywhere", () => {
    renderStep();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/No integrations/i)).not.toBeInTheDocument();
  });

  it("offers no git-host lanes — retired with GitHostCard", () => {
    renderStep();
    expect(screen.queryByRole("radio", { name: /Personal access token/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /GitHub App/ })).not.toBeInTheDocument();
  });
});
