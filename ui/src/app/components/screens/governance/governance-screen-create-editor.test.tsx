/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — the create editor on an empty list (#3052). Split from governance-screen.test.tsx,
// which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      previewGovernance: () => Promise.resolve({}),
      listChanges: () => Promise.resolve([]),
    },
  };
});
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: () => Promise.resolve(null) } };
});
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/policies")>("../../../lib/api/policies");
  return { ...actual, policies: { ...actual.policies, getDefaultPolicy: () => getDefaultPolicyMock() } };
});
vi.mock("../../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import type { GovernanceUnit, GovernanceSnapshot } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GovernanceScreen } from "./governance-screen";
import { aheadByHours } from "../../../lib/test-clock";

const PLATFORM: GovernanceUnit = {
  id: "p2",
  name: "Platform",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};

function renderScreen(snap: GovernanceSnapshot) {
  getGovernanceMock.mockResolvedValue(snap);
  render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator>
        <GovernanceScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getGovernanceMock.mockReset();
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC2" });
  window.localStorage.clear();
});

// The screen's ONE `default` button; a selected Segmented option is filled too but is a pressed toggle.
function tealButtons(): HTMLElement[] {
  return screen
    .getAllByRole("button")
    .filter((b) => !b.hasAttribute("aria-pressed") && b.className.split(/\s+/).includes("bg-primary"));
}

describe("GovernanceScreen — New profile on an empty list (#3052)", () => {
  it("the empty state is hidden while New profile's editor is open, and Cancel brings it back", async () => {
    renderScreen({ profiles: [], assignments: [] });
    await screen.findByText(GOV.EMPTY_TITLE);
    await userEvent.click(screen.getByRole("button", { name: GOV.NEW_CTA }));
    expect(await screen.findByTestId("governance-profile-editor")).toBeInTheDocument();
    expect(screen.queryByText(GOV.EMPTY_TITLE)).not.toBeInTheDocument();
    expect(screen.queryByText("WARDYN_DEFAULT_POLICY")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: PEOPLE.CANCEL }));
    expect(screen.queryByTestId("governance-profile-editor")).not.toBeInTheDocument();
    expect(await screen.findByText(GOV.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: GOV.NEW_CTA })).toBeEnabled();
    expect(tealButtons().map((b) => b.textContent)).toEqual([GOV.NEW_CTA]);
  });
});
