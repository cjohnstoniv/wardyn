/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The saved-policy lane (F2-F1/F2-F2/F2-F4/F2-F5) — split out of
// new-run-screen.test.tsx, which was already at the check-file-size.sh
// ceiling. Its own copy of the screen's mock harness: NewRunScreen imports
// policies/setup/health/runs/workspaces/capabilities regardless of which
// suite mounts it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: () => Promise.resolve({}) },
}));
vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ confinement_classes: ["CC1"] }) },
}));
const getDefaultPolicyMock = vi.fn();
const listPoliciesMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: (...a: unknown[]) => listPoliciesMock(...a),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    listRuns: () => Promise.resolve([]),
    preflightRun: vi.fn(),
    gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
  },
}));
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));
const myCapabilitiesMock = vi.fn();
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>(
    "../../../lib/capabilities",
  );
  return { ...actual, useMyCapabilities: (...a: unknown[]) => myCapabilitiesMock(...a) };
});

import { NewRunScreen } from "./new-run-screen";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN } from "../../wardyn/copy";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

function renderAsMember() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={false} securityOperator={false}>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
  listPoliciesMock.mockReset().mockResolvedValue([]);
  myCapabilitiesMock.mockReset().mockReturnValue(null);
});

describe("NewRunScreen — the saved-policy lane", () => {
  // A member's LIST read redacts secret refs (redactPoliciesForRead) — this is
  // what that redacted body looks like on the wire.
  const REDACTED_POLICY = {
    id: "pol_redacted",
    name: "Redacted policy",
    spec: {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review" as const,
      min_confinement_class: "CC1" as const,
      eligible_grants: [
        {
          kind: "api_key",
          scope: { host: "api.anthropic.com", secret_name: "<redacted>" },
          requires_approval: false,
        },
      ],
    },
  };

  async function pickSavedPolicy(name: string) {
    await user.click(await screen.findByRole("button", { name: /Reuse a saved policy/ }));
    await user.click(screen.getByRole("combobox", { name: "Saved policy" }));
    await user.click(await screen.findByRole("option", { name }));
  }

  // F2-F1 — a member's redacted body must never reach the wire just because
  // they looked at Custom after picking it.
  it("a member switching a redacted saved policy to Custom never ships the redacted body", async () => {
    listPoliciesMock.mockResolvedValue([REDACTED_POLICY]);
    renderAsMember();
    await pickSavedPolicy(REDACTED_POLICY.name);
    await user.click(screen.getByRole("button", { name: /Custom policy/ }));
    await user.type(screen.getByLabelText("Title"), "member custom");
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(JSON.stringify(createRunMock.mock.calls[0][0])).not.toContain("<redacted>");
  });

  // F2-F2 — the reworded rail sentence: the saved lane does NOT merge nothing,
  // the create door still prepends the attached Workspace card's mounts.
  it("the rail names the attached workspace as what still merges, not 'nothing'", async () => {
    listPoliciesMock.mockResolvedValue([REDACTED_POLICY]);
    renderScreen();
    await pickSavedPolicy(REDACTED_POLICY.name);
    expect(await screen.findByText(/Your attached workspace mounts into it/)).toBeInTheDocument();
    expect(screen.queryByText(/nothing on this page is merged into it/)).not.toBeInTheDocument();
  });

  // F2-F5 — a reference that no longer resolves gets its own reason, but only
  // once listPolicies() has actually answered (neg: no message before then).
  // A clone prefill is the one path that can carry a selectedPolicyId nothing
  // has loaded yet — every OTHER path only ever sets one off the loaded list.
  it("says the saved policy is gone once it fails to resolve — never before the list loads", async () => {
    let settle: (v: unknown) => void = () => {};
    listPoliciesMock.mockReturnValue(new Promise((r) => (settle = r)));
    render(
      <MemoryRouter
        initialEntries={[
          { pathname: "/runs/new", state: { prefill: { inlinePolicy: false, state: { selectedPolicyId: "pol_ghost" } } } },
        ]}
      >
        <OperatorProvider operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await screen.findByRole("combobox", { name: "Saved policy" });
    await user.type(screen.getByLabelText("Title"), "clone gone");
    expect(screen.queryByText(RUN.POLICY_GONE)).not.toBeInTheDocument();

    settle([]); // resolves with no match — NOW it is actually gone.
    expect(await screen.findByText(RUN.POLICY_GONE)).toBeInTheDocument();
  });

  // F2-F4 — codex-cli has no tool-approval contract; the Seg must show "Auto"
  // checked, DERIVED for display, never patched into state.
  it("shows Auto for codex-cli and restores the real hold choice on switch-back", async () => {
    renderScreen();
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("radio", { name: /^Hold in Wardyn/ }));
    expect(screen.getByRole("radio", { name: /^Hold in Wardyn/ })).toHaveAttribute("aria-checked", "true");

    await user.click(screen.getByRole("combobox", { name: "Agent" }));
    await user.click(await screen.findByRole("option", { name: "Codex CLI" }));
    expect(screen.getByRole("radio", { name: /^Auto —/ })).toHaveAttribute("aria-checked", "true");

    // neg: switching back must restore "hold" — an effect that had patched
    // state.toolApprovals to "auto" would have clobbered it permanently.
    await user.click(screen.getByRole("combobox", { name: "Agent" }));
    await user.click(await screen.findByRole("option", { name: "Claude Code" }));
    expect(screen.getByRole("radio", { name: /^Hold in Wardyn/ })).toHaveAttribute("aria-checked", "true");
  });
});
