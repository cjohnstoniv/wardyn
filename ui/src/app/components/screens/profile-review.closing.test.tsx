/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Closing the sheet (no run) keeps what it shows while it slides out. jsdom runs no CSS animation, so a closed sheet
// unmounts at once there; this file gives the sheet's content the exit animation a browser runs, which holds it
// mounted, closing, the way a person sees it.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ProfileProposal } from "../../lib/types";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const profileRunMock = vi.fn<(runId: string) => Promise<ProfileProposal>>();
vi.mock("../../lib/api/runs", () => ({ runs: { profileRun: (runId: string) => profileRunMock(runId) } }));
vi.mock("../../lib/api/policies", () => ({ policies: { createPolicy: vi.fn() } }));

import { ProfileReview } from "./profile-review";

const PROPOSAL: ProfileProposal = {
  kind: "profile_proposal",
  proposed: {
    run: { agent: "claude-code", repo: "acme/api", task: "recorded session", confinement_class: "CC2", interactive: true },
    inline_policy: { allowed_domains: ["github.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2", eligible_grants: [] },
  },
  risk_assessment: [],
  overall_risk: "low",
  observations: { domains: [], minted_grant_ids: [], exec_argv0s: [], file_writes: [], connects: [], anomalies: [] },
  warnings: [],
};

// The sheet content's animation name, as its open and closed classes set it in a browser.
beforeEach(() => {
  const real = window.getComputedStyle.bind(window);
  vi.spyOn(window, "getComputedStyle").mockImplementation((el, pseudo) => {
    const style = real(el, pseudo);
    if (!(el instanceof HTMLElement) || el.dataset.slot !== "sheet-content") return style;
    return new Proxy(style, {
      get: (target, key) => {
        if (key === "animationName") return el.dataset.state === "closed" ? "slide-out" : "slide-in";
        const value: unknown = Reflect.get(target, key);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
  });
  profileRunMock.mockReset().mockResolvedValue(PROPOSAL);
});
afterEach(() => vi.restoreAllMocks());

describe("ProfileReview: closing", () => {
  it("keeps the proposal on the sheet while it slides out, never its loading state", async () => {
    const { rerender } = render(<ProfileReview runId="run-a" onClose={() => {}} />);
    await screen.findByText("Overall risk");
    rerender(<ProfileReview runId={null} onClose={() => {}} />);
    expect(document.querySelector('[data-slot="sheet-content"]')?.getAttribute("data-state")).toBe("closed");
    expect(screen.getByText("Overall risk")).toBeInTheDocument();
  });
});
