/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { StepEgress } from "./step-egress";
import { initialWizardState } from "./wizard-types";
import type { Workspace } from "../../../lib/types";

// D6/claim3: buildSpec unions grant-implied hosts into allowed_domains that
// never appeared as a chip on this step — a repo workspace pick or an LLM key
// silently widened egress with no control here naming it. These render as
// non-removable muted chips so the step stops disagreeing with what buildSpec
// actually sends (wizard-types.test.ts covers the underlying predicate).
describe("StepEgress — 'Added by grants:' row (D6/claim3)", () => {
  function renderStep(overrides?: Partial<Parameters<typeof StepEgress>[0]>) {
    return render(
      <StepEgress
        state={initialWizardState()}
        patch={() => {}}
        workspaces={[] as Workspace[]}
        {...overrides}
      />,
    );
  }

  it("renders nothing when no grant implies a host", () => {
    renderStep();
    expect(screen.queryByTestId("egress-implied-hosts")).toBeNull();
  });

  it("shows the model-key host once an LLM secret is selected", () => {
    renderStep({ state: { ...initialWizardState(), llmSecretName: "anthropic-api-key" } });
    const row = screen.getByTestId("egress-implied-hosts");
    expect(within(row).getByText("api.anthropic.com")).toBeInTheDocument();
  });

  it("shows the GitHub hosts, un-removable (no remove control unlike the allow-list pills), when the GitHub grant is on", () => {
    renderStep({ state: { ...initialWizardState(), githubEnabled: true } });
    const row = screen.getByTestId("egress-implied-hosts");
    expect(within(row).getByText("github.com")).toBeInTheDocument();
    expect(within(row).getByText("*.githubusercontent.com")).toBeInTheDocument();
    expect(row.querySelector("button")).toBeNull();
  });

  it("names a repo-workspace selection's implied hosts even with the GitHub grant untouched (claim 3's sharpest sub-case)", () => {
    const repoWs: Workspace = {
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      status: "scanned",
      created_at: "",
      updated_at: "",
    };
    renderStep({
      state: { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] },
      workspaces: [repoWs],
    });
    const row = screen.getByTestId("egress-implied-hosts");
    expect(within(row).getByText("github.com")).toBeInTheDocument();
  });

  it("is hidden under allow-all egress, matching the Allowed-domains grid it sits under", () => {
    renderStep({ state: { ...initialWizardState(), githubEnabled: true, allowAllEgress: true } });
    expect(screen.queryByTestId("egress-implied-hosts")).toBeNull();
  });
});

// ui-newrun-3: the preset-domain toggle chips signaled on/off state via
// color classes only (no aria-pressed), unlike every sibling toggle-card on
// this screen (e.g. step-confinement.tsx's OptionCard).
describe("StepEgress — preset chip aria-pressed (ui-newrun-3)", () => {
  it("reflects the toggled-on/off state via aria-pressed, not color alone", () => {
    render(
      <StepEgress
        state={{ ...initialWizardState(), allowedDomains: ["api.anthropic.com"] }}
        patch={() => {}}
        workspaces={[] as Workspace[]}
      />,
    );
    // api.anthropic.com is a PRESET_DOMAINS entry and is in allowedDomains above.
    expect(screen.getByRole("button", { name: "api.anthropic.com" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    // github.com is a PRESET_DOMAINS entry NOT in allowedDomains.
    expect(screen.getByRole("button", { name: "github.com" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });
});
