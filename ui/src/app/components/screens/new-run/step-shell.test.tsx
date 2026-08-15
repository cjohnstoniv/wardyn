/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StepIndicator } from "./step-shell";

// ui-newrun-2 / ui-wsWizard-7: the step rail's active/done/todo state was
// color-only (no ARIA state) and the visible label is `hidden sm:inline`
// (removed from the accessibility tree below sm, not just unpainted) with no
// aria-label fallback — so a screen reader, or a narrow viewport, only ever
// heard/saw a bare ordinal ("1", "2", …) for every step, active or not.
describe("StepIndicator — step rail accessibility", () => {
  const steps = [
    { id: "a", label: "Basics" },
    { id: "b", label: "Access" },
    { id: "c", label: "Egress" },
  ];

  it("marks the active step with aria-current=step and no others", () => {
    render(<StepIndicator current="b" steps={steps} />);
    expect(screen.getByRole("button", { name: /access/i })).toHaveAttribute("aria-current", "step");
    expect(screen.getByRole("button", { name: /^basics/i })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("button", { name: /^egress/i })).not.toHaveAttribute("aria-current");
  });

  it("carries the step name in aria-label so it survives the sm:inline label collapse", () => {
    render(<StepIndicator current="a" steps={steps} />);
    // The visible label span is `hidden sm:inline` — accessible name must NOT
    // depend on it, so query by the accessible name directly.
    expect(screen.getByRole("button", { name: /basics/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /access/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /egress/i })).toBeInTheDocument();
  });

  it("distinguishes done vs active state in the accessible name, not just color", () => {
    render(<StepIndicator current="b" steps={steps} />);
    expect(screen.getByRole("button", { name: /basics.*completed/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /access.*current step/i })).toBeInTheDocument();
  });
});
