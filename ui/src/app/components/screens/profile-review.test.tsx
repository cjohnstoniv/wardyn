/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ProfileProposal } from "../../lib/types";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: {
    success: (...a: unknown[]) => toastSuccess(...a),
    error: (...a: unknown[]) => toastError(...a),
  },
}));

const profileRunMock = vi.fn();
const createPolicyMock = vi.fn();
vi.mock("../../lib/api", () => ({
  api: {
    profileRun: (...a: unknown[]) => profileRunMock(...a),
    createPolicy: (...a: unknown[]) => createPolicyMock(...a),
  },
}));

import { ProfileReview } from "./profile-review";

function proposal(overrides: Partial<ProfileProposal> = {}): ProfileProposal {
  return {
    kind: "profile_proposal",
    proposed: {
      run: {
        agent: "claude-code",
        repo: "acme/api",
        task: "recorded session",
        confinement_class: "CC2",
        interactive: true,
      },
      inline_policy: {
        allowed_domains: ["api.anthropic.com", "github.com"],
        first_use_approval: "deny_with_review",
        min_confinement_class: "CC2",
        eligible_grants: [{ kind: "github_token", requires_approval: true }],
      },
    },
    risk_assessment: [
      { field: "allow_all_egress", value: "false", risk_level: "low", rationale: "Scoped egress." },
    ],
    overall_risk: "low",
    observations: {
      domains: [
        { host: "api.anthropic.com", methods: ["POST"], allow_count: 12, deny_count: 0, pending_count: 0 },
        { host: "evil.example.com", methods: ["GET"], allow_count: 0, deny_count: 3, pending_count: 1 },
      ],
      minted_grant_ids: ["grant-1"],
      exec_argv0s: ["git", "node"],
      file_writes: ["/home/agent/work/main.go"],
      connects: ["10.0.0.5:443"],
      anomalies: ["denied host evil.example.com retried 3x"],
    },
    warnings: ["clamped min_confinement_class to CC2"],
    ...overrides,
  };
}

describe("ProfileReview", () => {
  beforeEach(() => {
    toastSuccess.mockReset();
    toastError.mockReset();
    profileRunMock.mockReset();
    createPolicyMock.mockReset();
    profileRunMock.mockResolvedValue(proposal());
    createPolicyMock.mockResolvedValue({ id: "pol-1" });
  });

  it("synthesizes a profile for the run and renders the observations", async () => {
    render(<ProfileReview runId="run-1" onClose={() => {}} />);

    // Calls POST /runs/{id}/profile for the given run.
    await waitFor(() => expect(profileRunMock).toHaveBeenCalledWith("run-1"));

    // Overall risk + a graded risk row render (clone of compose-review).
    expect(await screen.findByText("Overall risk")).toBeInTheDocument();
    expect(screen.getByText("allow_all_egress")).toBeInTheDocument();

    // Observations: egress domains with methods + decision tallies.
    const obs = screen.getByLabelText("Observations");
    expect(within(obs).getByText("evil.example.com")).toBeInTheDocument();
    expect(within(obs).getByText("12 allow")).toBeInTheDocument();
    expect(within(obs).getByText("3 deny")).toBeInTheDocument();
    expect(within(obs).getByText("1 pending")).toBeInTheDocument();
    expect(within(obs).getByText("POST")).toBeInTheDocument();

    // exec argv0s, file writes, connects.
    expect(within(obs).getByText("git")).toBeInTheDocument();
    expect(within(obs).getByText("/home/agent/work/main.go")).toBeInTheDocument();
    expect(within(obs).getByText("10.0.0.5:443")).toBeInTheDocument();

    // Anomalies are highlighted in their own section.
    const anomalies = screen.getByTestId("profile-anomalies");
    expect(within(anomalies).getByText(/retried 3x/)).toBeInTheDocument();

    // Clamp warning surfaces.
    expect(screen.getByText(/clamped min_confinement_class/)).toBeInTheDocument();
  });

  it("renders a retryable error when synthesis fails", async () => {
    profileRunMock.mockReset();
    profileRunMock.mockRejectedValue(new Error("boom"));
    render(<ProfileReview runId="run-err" onClose={() => {}} />);
    expect(await screen.findByText(/couldn't synthesize a profile/i)).toBeInTheDocument();
  });

  it("Save as policy prompts for a name and POSTs the proposed inline_policy", async () => {
    render(<ProfileReview runId="run-1" onClose={() => {}} />);
    await screen.findByText("Overall risk");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /save as policy/i }));

    // The name dialog appears, prefilled; rename and save.
    const nameField = await screen.findByLabelText(/policy name/i);
    await user.clear(nameField);
    await user.type(nameField, "acme-recorded");
    await user.click(screen.getByRole("button", { name: /^save policy$/i }));

    await waitFor(() => expect(createPolicyMock).toHaveBeenCalledTimes(1));
    expect(createPolicyMock.mock.calls[0][0]).toBe("acme-recorded");
    // The SPEC sent is the proposal's inline_policy verbatim.
    expect(createPolicyMock.mock.calls[0][1]).toMatchObject({
      min_confinement_class: "CC2",
      allowed_domains: ["api.anthropic.com", "github.com"],
    });
    expect(toastSuccess).toHaveBeenCalled();
  });

  it("Save as is persists directly under the workspace+recording name (no dialog)", async () => {
    const onClose = vi.fn();
    render(<ProfileReview runId="run-1" suggestedName="acme-api-build-test" onClose={onClose} />);
    await screen.findByText("Overall risk");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByTestId("profile-save-as-is"));

    await waitFor(() => expect(createPolicyMock).toHaveBeenCalledTimes(1));
    expect(createPolicyMock.mock.calls[0][0]).toBe("acme-api-build-test");
    expect(createPolicyMock.mock.calls[0][1]).toMatchObject({
      allowed_domains: ["api.anthropic.com", "github.com"],
    });
    expect(toastSuccess).toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled(); // closes the drawer on success
  });
});
