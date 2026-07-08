/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { RiskBadge } from "./primitives";
import type { RiskLevel } from "../../lib/types";

// The composer risk badge must render a distinct, escalating tone per level so a
// high-risk choice is visually unmistakable, and fail-soft on an unknown level
// (same posture as the other enum badges). The tone is carried by the inner
// Chip's tone class (success/warning/danger); we assert on those + the label +
// the data-risk hook used by the review screen and e2e selectors.
describe("RiskBadge", () => {
  const cases: { level: RiskLevel; label: string; toneClass: string }[] = [
    { level: "low", label: "Low", toneClass: "text-success" },
    { level: "medium", label: "Medium", toneClass: "text-warning" },
    { level: "high", label: "High", toneClass: "text-danger" },
  ];

  it.each(cases)("renders the $level level with its escalating tone", ({ level, label, toneClass }) => {
    const { container } = render(<RiskBadge level={level} />);
    // The human label.
    expect(screen.getByText(label)).toBeInTheDocument();
    // A stable data hook carrying the raw level.
    expect(container.querySelector(`[data-risk="${level}"]`)).not.toBeNull();
    // The per-level tone (success/warning/danger) is applied to the chip.
    expect(container.querySelector(`.${toneClass.replace(/\//g, "\\/")}`)).not.toBeNull();
  });

  it("escalates tone with risk (low=success, medium=warning, high=danger) and they differ", () => {
    const tones = cases.map(({ level }) => {
      const { container } = render(<RiskBadge level={level} />);
      const chip = container.querySelector("[data-risk] > span");
      return chip?.className ?? "";
    });
    expect(tones[0]).toContain("text-success");
    expect(tones[1]).toContain("text-warning");
    expect(tones[2]).toContain("text-danger");
    // All three tones are distinct.
    expect(new Set(tones).size).toBe(3);
  });

  it("fail-soft: an unknown level does not throw and shows the raw value", () => {
    const unknown = "critical" as RiskLevel;
    expect(() => render(<RiskBadge level={unknown} />)).not.toThrow();
    expect(screen.getByText("critical")).toBeInTheDocument();
  });
});
