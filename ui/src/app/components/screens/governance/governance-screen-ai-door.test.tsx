/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — the Draft with AI door follows the profile being edited: a new draft with none open, that profile's
// own with one open. Split from governance-screen.test.tsx, which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
const listChangesMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      previewGovernance: () => Promise.resolve({}),
      listChanges: () => listChangesMock(),
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
// The assistant is ready, so the door is a link.
vi.mock("../../../lib/api/assistant", () => ({ assistant: { getMyAssistant: () => Promise.resolve({ state: "ready" }) } }));

import type { GovernanceUnit, GovernanceSnapshot } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { composerHref } from "../../../lib/composer-url";
import { ASSISTANT } from "../../wardyn/copy/assistant";
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
const SNAP: GovernanceSnapshot = { profiles: [PLATFORM], assignments: [] };

function renderScreen() {
  getGovernanceMock.mockResolvedValue(SNAP);
  listChangesMock.mockResolvedValue([]);
  getDefaultPolicyMock.mockResolvedValue(null);
  render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator>
        <GovernanceScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => vi.clearAllMocks());

describe("GovernanceScreen: the Draft with AI door and the profile being edited", () => {
  it("opens a new draft with no profile open, and that profile's own draft once its editor is open", async () => {
    renderScreen();
    const door = () => screen.findByRole("link", { name: ASSISTANT.DRAFT_WITH_AI });
    expect(await door()).toHaveAttribute("href", composerHref({ kind: "unit" }));
    await userEvent.click(await screen.findByRole("button", { name: `${GOV.EDIT} ${PLATFORM.name}` }));
    expect(await door()).toHaveAttribute("href", composerHref({ kind: "unit", id: PLATFORM.id }));
  });
});
