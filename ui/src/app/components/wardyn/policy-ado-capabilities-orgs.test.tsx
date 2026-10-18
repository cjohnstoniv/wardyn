/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { RunPolicySpec } from "../../lib/types";
import { ADO_ACCESS } from "../../lib/ado-access-copy";
import { orgCeilings, unionCeiling } from "../../lib/ado-ceilings";
import { ADOCapabilitiesSection } from "./policy-ado-capabilities";

const SPEC: RunPolicySpec = { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" };
const rows = [
  { state: "live", org: "https://dev.azure.com/acme", capability_ceiling: ["code_read", "pr"] },
  { state: "live", org: "https://dev.azure.com/beta", capability_ceiling: ["code_read"] },
];

// #2146: a capability is locked only when no organisation allows it.
describe("ADOCapabilitiesSection across organisations", () => {
  const cs = orgCeilings(rows);

  function renderIt() {
    render(<ADOCapabilitiesSection spec={SPEC} ceiling={unionCeiling(cs)} orgCeilings={cs} onSpecChange={() => {}} />);
  }

  it("leaves a capability one organisation allows open, and says who does not allow it", () => {
    renderIt();
    expect(screen.getByRole("checkbox", { name: /^Contribute to pull requests/ })).toBeEnabled();
    expect(screen.getByTestId("ado-cap-hint-pr")).toHaveTextContent(ADO_ACCESS.NOT_ALLOWED_FOR("beta"));
    expect(ADO_ACCESS.NOT_ALLOWED_FOR("beta")).toBe("Not allowed for beta.");
  });

  it("locks a capability no organisation allows, with no hint", () => {
    renderIt();
    expect(screen.getByRole("checkbox", { name: /^View work items/ })).toBeDisabled();
    expect(screen.queryByTestId("ado-cap-hint-work_read")).not.toBeInTheDocument();
  });

  it("says nothing about a capability every organisation allows", () => {
    renderIt();
    expect(screen.queryByTestId("ado-cap-hint-code_read")).not.toBeInTheDocument();
  });
});
