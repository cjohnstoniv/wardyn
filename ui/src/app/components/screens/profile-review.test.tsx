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
vi.mock("../../lib/api/runs", () => ({
  runs: { profileRun: (...a: unknown[]) => profileRunMock(...a) },
}));
vi.mock("../../lib/api/policies", () => ({
  policies: { createPolicy: (...a: unknown[]) => createPolicyMock(...a) },
}));
const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
vi.mock("../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      listUserTypes: async () => [],
    },
  };
});

import { HttpError } from "../../lib/api/core";
import { AVAILABILITY } from "../../lib/availability-copy";
import { ProfileReview } from "./profile-review";
import { setField } from "../../../test/set-field";

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
    getAvailabilityMock.mockReset();
    putAvailabilityMock.mockReset();
    upsertGrantMock.mockReset();
    getAvailabilityMock.mockImplementation(async (kind: string, value: string) => ({
      kind,
      value,
      restricted: false,
      allowed_by: [],
    }));
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

  // R-03: allowed_domains is `null` on the wire (not `[]`) when the source
  // recording had no observed egress — internal/types/policy.go's
  // AllowedDomains has no `,omitempty`, so the key is always present but a
  // nil slice marshals to `null`. Must not throw and must render "0 allowed".
  it("renders a synthesized profile with allowed_domains: null without throwing", async () => {
    profileRunMock.mockReset();
    profileRunMock.mockResolvedValue(
      proposal({
        proposed: {
          run: proposal().proposed.run,
          inline_policy: { ...proposal().proposed.inline_policy, allowed_domains: null },
        },
      }),
    );
    render(<ProfileReview runId="run-null-domains" onClose={() => {}} />);
    expect(await screen.findByText("Overall risk")).toBeInTheDocument();
    expect(screen.getByText("0 allowed")).toBeInTheDocument();
  });

  it("renders a retryable error when synthesis fails", async () => {
    profileRunMock.mockReset();
    profileRunMock.mockRejectedValue(new Error("boom"));
    render(<ProfileReview runId="run-err" onClose={() => {}} />);
    expect(await screen.findByText(/couldn't synthesize a profile/i)).toBeInTheDocument();
  });

  it("Save as policy prompts for a name and POSTs the proposed inline_policy", async () => {
    const onClose = vi.fn();
    render(<ProfileReview runId="run-1" onClose={onClose} />);
    await screen.findByText("Overall risk");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /save as policy/i }));

    // The name dialog appears, prefilled; rename and save.
    const nameField = await screen.findByLabelText(/policy name/i);
    await user.clear(nameField);
    setField(nameField, "acme-recorded");
    await user.click(screen.getByRole("button", { name: /^save policy$/i }));

    await waitFor(() => expect(createPolicyMock).toHaveBeenCalledTimes(1));
    expect(createPolicyMock.mock.calls[0][0]).toBe("acme-recorded");
    // The SPEC sent is the proposal's inline_policy verbatim.
    expect(createPolicyMock.mock.calls[0][1]).toMatchObject({
      min_confinement_class: "CC2",
      allowed_domains: ["api.anthropic.com", "github.com"],
    });
    expect(toastSuccess).toHaveBeenCalled();

    // The named-save path must end like "Save as is": the drawer closes, so it stops
    // telling the operator "nothing is created until you save it" about a policy that
    // now exists. The dialog goes with it.
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(screen.queryByLabelText(/policy name/i)).not.toBeInTheDocument();
  });

  it("the sheet foot has one Save as policy; Save as is and Save as with a name are gone (#2514)", async () => {
    render(<ProfileReview runId="run-1" suggestedName="acme-api-build-test" onClose={() => {}} />);
    await screen.findByText("Overall risk");

    expect(screen.getAllByRole("button", { name: /save as/i })).toHaveLength(1);
    expect(screen.queryByText(/save as is/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/with a name/i)).not.toBeInTheDocument();
  });

  it("opens the dialog with the recording's name and Available to at Everyone; Save policy writes the list after the policy (#2514)", async () => {
    const onClose = vi.fn();
    createPolicyMock.mockResolvedValue({ id: "pol-1" });
    render(<ProfileReview runId="run-1" suggestedName="acme-api-build-test" onClose={onClose} />);
    await screen.findByText("Overall risk");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^save as policy$/i }));
    const dialog = await screen.findByRole("dialog", { name: "Save as policy" });
    expect(within(dialog).getByLabelText(/policy name/i)).toHaveValue("acme-api-build-test");
    expect(within(dialog).getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    expect(within(dialog).getByTestId("availability-only").textContent).toBe(AVAILABILITY.POLICY_ONLY_HINT);
    expect(within(dialog).getByText(AVAILABILITY.POLICY_NOTE)).toBeInTheDocument();

    await user.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    // Only these with nobody listed holds Save policy shut.
    expect(within(dialog).getByRole("button", { name: /^save policy$/i })).toBeDisabled();
    setField(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "team-docs");
    await user.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await user.click(within(dialog).getByRole("button", { name: /^save policy$/i }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(createPolicyMock.mock.calls[0][0]).toBe("acme-api-build-test");
    expect(upsertGrantMock).toHaveBeenCalledWith(
      expect.objectContaining({ subject: "team-docs", capability: "policy", value: "pol-1", effect: "allow" }),
    );
    expect(putAvailabilityMock).toHaveBeenCalledWith("policy", "pol-1", true);
  });

  it("a refused list write keeps the dialog open with the three-part alert and the live control (#2514)", async () => {
    const onClose = vi.fn();
    createPolicyMock.mockResolvedValue({ id: "pol-1" });
    upsertGrantMock.mockRejectedValue(new HttpError(400, "That group doesn't exist."));
    render(<ProfileReview runId="run-1" suggestedName="acme-api-build-test" onClose={onClose} />);
    await screen.findByText("Overall risk");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^save as policy$/i }));
    const dialog = await screen.findByRole("dialog", { name: "Save as policy" });
    await user.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    setField(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "team-docs");
    await user.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await user.click(within(dialog).getByRole("button", { name: /^save policy$/i }));

    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent(AVAILABILITY.CREATE_PARTIAL_TITLE);
    expect(alert).toHaveTextContent("That group doesn't exist.");
    expect(alert).toHaveTextContent(AVAILABILITY.CREATE_PARTIAL_EVERYONE);
    expect(onClose).not.toHaveBeenCalled();
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalledWith("policy", "pol-1"));
    expect(createPolicyMock).toHaveBeenCalledTimes(1);
  });
});
