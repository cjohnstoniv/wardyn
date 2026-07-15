/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { PreflightResult } from "../../../lib/types";
import { StepReview } from "./step-review";
import { initialWizardState } from "./wizard-types";

// StepReview is a pure display component, so these render it directly with props
// (the wizard.test.tsx integration proves preflight is actually FIRED on Review).

function preflight(over: Partial<PreflightResult> = {}): PreflightResult {
  return {
    enforced_confinement_class: "CC2",
    setup_items: [
      {
        id: "backend:CC2",
        kind: "backend",
        label: "Sandbox barrier: Wall",
        required_by: "the proposal's confinement class",
        status: "satisfied",
      },
    ],
    ...over,
  };
}

describe("StepReview — preflight surfacing", () => {
  it("renders the setup checklist rows from the preflight result", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.getByTestId("preflight-checklist")).toBeInTheDocument();
    expect(screen.getByTestId("setup-item-backend:CC2")).toBeInTheDocument();
  });

  it("shows a quiet 'preflight unavailable' line on error, never blocking Review", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={null}
        preflightStatus="error"
      />,
    );
    expect(screen.getByTestId("preflight-unavailable")).toBeInTheDocument();
    // The inline_policy summary still renders — Review is never blocked.
    expect(screen.getByText(/inline_policy \(sent verbatim\)/i)).toBeInTheDocument();
  });

  it("renders the silent-raise line (friendly tier name) when the enforced class exceeds the pick", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({ enforced_confinement_class: "CC3" })}
        preflightStatus="idle"
      />,
    );
    const line = screen.getByTestId("preflight-cc-raise");
    expect(line).toHaveTextContent(/Launches at Vault/i);
    expect(line).toHaveTextContent(/write-capable or third-party production credentials/i);
    // Never the raw wire code (e2e enforces UI copy uses Fence/Wall/Vault).
    expect(line).not.toHaveTextContent(/CC3/);
  });

  it("does NOT show the raise line when the enforced class equals the pick", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({ enforced_confinement_class: "CC2" })}
        preflightStatus="idle"
      />,
    );
    expect(screen.queryByTestId("preflight-cc-raise")).toBeNull();
  });

  it("adds the autonomous-run wording to the no-model warning in batch mode", () => {
    // Default state: claude-code + apikey + no stored secret => noLlmCred.
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "batch" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.getByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.getByTestId("review-batch-no-model")).toHaveTextContent(
      /autonomous run can't perform its task without model access/i,
    );
  });

  it("omits the autonomous wording for an interactive run", () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "interactive" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    // The no-model warning still shows (no LLM cred), but without the batch clause.
    expect(screen.getByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.queryByTestId("review-batch-no-model")).toBeNull();
  });
});
