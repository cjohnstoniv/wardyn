/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";

const gradePolicyMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: (...a: unknown[]) => gradePolicyMock(...a) },
}));

import { SafetyMeter, safetyLabel } from "./safety-meter";
import type { PolicyGrade, RunPolicySpec } from "../../lib/types";

// Real-shaped composer.Grade outputs — the exact field/value/rationale strings
// internal/composer/risk.go emits — one per reachable label. These are the four
// template-click destinations the plan walked through Grade: click minimal →
// Guarded (CC2), edit CC3 → Safest, edit CC1 → Elevated, click allow-all →
// Weakest (allow-all egress + omitted idle cap = two highs).
const SAFEST: PolicyGrade = {
  overall_risk: "low",
  risk_assessment: [
    {
      field: "min_confinement_class",
      value: "CC3",
      risk_level: "low",
      rationale: "Vault (the strongest tier — a hardware-isolated Kata VM).",
      invariant_ref: "5",
    },
    {
      field: "allowed_domains",
      value: "api.anthropic.com",
      risk_level: "low",
      rationale: "Default-deny egress limited to baseline coding-agent hosts.",
      invariant_ref: "3",
    },
  ],
};

const GUARDED: PolicyGrade = {
  overall_risk: "medium",
  risk_assessment: [
    {
      field: "min_confinement_class",
      value: "CC2",
      risk_level: "medium",
      rationale: "Wall (the default tier — a gVisor sandbox).",
      invariant_ref: "5",
    },
    {
      field: "allowed_domains",
      value: "api.anthropic.com",
      risk_level: "low",
      rationale: "Default-deny egress limited to baseline coding-agent hosts.",
      invariant_ref: "3",
    },
  ],
};

// overall high with EXACTLY ONE high item (Grade sorts riskiest-first).
const ELEVATED: PolicyGrade = {
  overall_risk: "high",
  risk_assessment: [
    {
      field: "min_confinement_class",
      value: "CC1",
      risk_level: "high",
      rationale:
        "Fence (the weakest tier — a hardened shared-kernel container): a kernel-level escape is not contained by a second boundary.",
      invariant_ref: "5",
    },
    {
      field: "allowed_domains",
      value: "api.anthropic.com",
      risk_level: "low",
      rationale: "Default-deny egress limited to baseline coding-agent hosts.",
      invariant_ref: "3",
    },
  ],
};

// overall high with TWO high items.
const WEAKEST: PolicyGrade = {
  overall_risk: "high",
  risk_assessment: [
    {
      field: "allow_all_egress",
      value: "true",
      risk_level: "high",
      rationale:
        "Allow-all egress lets the agent reach ANY public host (deny-list only). Exfiltration surface is maximal; private/metadata IPs are still blocked structurally.",
      invariant_ref: "3",
    },
    {
      field: "auto_stop_after_sec",
      value: "0",
      risk_level: "high",
      rationale:
        "Auto-stop not configured (field omitted or 0): this run is never reaped and holds its minted credentials indefinitely.",
      invariant_ref: "6",
    },
    {
      field: "min_confinement_class",
      value: "CC2",
      risk_level: "medium",
      rationale: "Wall (the default tier — a gVisor sandbox).",
      invariant_ref: "5",
    },
  ],
};

const SPEC: RunPolicySpec = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
};

beforeEach(() => {
  gradePolicyMock.mockReset();
  gradePolicyMock.mockResolvedValue(GUARDED);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("safetyLabel — all four labels reachable from real Grade outputs", () => {
  it("overall low → Safest", () => {
    expect(safetyLabel(SAFEST)).toBe("Safest");
  });
  it("overall medium → Guarded", () => {
    expect(safetyLabel(GUARDED)).toBe("Guarded");
  });
  it("overall high with exactly one high item → Elevated", () => {
    expect(safetyLabel(ELEVATED)).toBe("Elevated");
  });
  it("overall high with two or more high items → Weakest", () => {
    expect(safetyLabel(WEAKEST)).toBe("Weakest");
  });
});

describe("SafetyMeter — render + label", () => {
  it("renders the derived label once the grade resolves", async () => {
    gradePolicyMock.mockResolvedValue(WEAKEST);
    render(<SafetyMeter spec={SPEC} />);
    const meter = screen.getByTestId("safety-meter");
    await waitFor(() => expect(meter).toHaveAttribute("data-safety", "Weakest"), { timeout: 2000 });
    expect(meter).toHaveTextContent("Weakest");
  });

  it("forwards the interactive hint to the grade call", async () => {
    render(<SafetyMeter spec={SPEC} interactive />);
    await waitFor(() => expect(gradePolicyMock).toHaveBeenCalled(), { timeout: 2000 });
    expect(gradePolicyMock).toHaveBeenLastCalledWith(expect.any(Object), true);
  });
});

describe("SafetyMeter — debounce", () => {
  it("makes no call before 500ms, exactly one after", async () => {
    vi.useFakeTimers();
    render(<SafetyMeter spec={SPEC} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(499);
    });
    expect(gradePolicyMock).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(gradePolicyMock).toHaveBeenCalledTimes(1);
  });

  it("coalesces rapid edits into one call with the LATEST spec", async () => {
    vi.useFakeTimers();
    const A: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
    };
    const B: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC3",
    };
    const { rerender } = render(<SafetyMeter spec={A} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200);
    });
    rerender(<SafetyMeter spec={B} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(gradePolicyMock).toHaveBeenCalledTimes(1);
    expect(gradePolicyMock).toHaveBeenLastCalledWith(B, undefined);
  });
});

describe("SafetyMeter — dimmed on parse failure", () => {
  it("dims and never calls the grader when the spec is null", () => {
    render(<SafetyMeter spec={null} />);
    const meter = screen.getByTestId("safety-meter");
    expect(meter).toHaveAttribute("data-safety", "");
    expect(meter).toHaveTextContent(/fix the json first/i);
    // The effect returns before scheduling anything, so no call is ever queued.
    expect(gradePolicyMock).not.toHaveBeenCalled();
  });
});
