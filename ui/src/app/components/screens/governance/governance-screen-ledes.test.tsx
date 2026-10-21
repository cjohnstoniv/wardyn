/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — what each section says once, and where the longer text lives (#3049, #3060). Split from
// governance-screen.test.tsx, which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
vi.mock("../../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import type { GovernanceProfile, GovernanceSnapshot } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GovernanceScreen } from "./governance-screen";
import { aheadByHours } from "../../../lib/test-clock";

const PLATFORM: GovernanceProfile = {
  id: "p2",
  name: "Platform",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};
const WITH_ROWS: GovernanceSnapshot = {
  profiles: [PLATFORM],
  assignments: [{ id: "a1", subject_type: "group", subject: "wardyn.platform", profile_id: PLATFORM.id, priority: 10, created_at: aheadByHours(-1) }],
};

function renderScreen(snap: GovernanceSnapshot = WITH_ROWS) {
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
  window.localStorage.clear();
});

describe("GovernanceScreen — the page lede (#3060)", () => {
  it("is one sentence, the rest in a closed How governance works", async () => {
    renderScreen();
    await screen.findByRole("heading", { name: GOV.TITLE, level: 1 });
    expect(screen.getByTestId("page-header-description")).toHaveTextContent(GOV.LEAD);
    expect(GOV.LEAD.length).toBeLessThanOrEqual(120);
    const how = screen.getByTestId("how-governance");
    expect(how).not.toHaveAttribute("open");
    expect(within(how).getByText(GOV.HOW_TITLE)).toBeInTheDocument();
    expect(within(how).getByText(GOV.HOW_BODY)).toBeInTheDocument();
  });
});

describe("GovernanceScreen — assignments say each thing once (#3049)", () => {
  it("with rows: the lede, and the precedence rule and both halves of the effect fact in a closed disclosure", async () => {
    renderScreen();
    await screen.findByText(GOV.ASSIGN_TITLE);
    expect(screen.getByText(GOV.ASSIGN_LEAD)).toBeInTheDocument();
    expect(screen.queryByText(GOV.EMPTY_ASSIGN_TITLE)).toBeNull();
    const combine = screen.getByTestId("how-governance-assignments");
    expect(combine).not.toHaveAttribute("open");
    expect(within(combine).getByText(GOV.COMBINE_TITLE)).toBeInTheDocument();
    expect(within(combine).getByText(GOV.PRECEDENCE)).toBeInTheDocument();
    expect(within(combine).getByText(GOV.EFFECT_NOTE)).toBeInTheDocument();
    expect(within(combine).getByText(GOV.SIGNIN_NOTE)).toBeInTheDocument();
  });

  it("empty: no lede, the empty state and the form, and the same closed disclosure under the form", async () => {
    renderScreen({ ...WITH_ROWS, assignments: [] });
    await screen.findByText(GOV.EMPTY_ASSIGN_TITLE);
    expect(screen.queryByText(GOV.ASSIGN_LEAD)).toBeNull();
    const combine = screen.getByTestId("how-governance-assignments");
    expect(combine).not.toHaveAttribute("open");
    expect(within(combine).getByText(GOV.PRECEDENCE)).toBeInTheDocument();
    const form = screen.getByRole("heading", { name: GOV.ADD_TITLE });
    expect(form.compareDocumentPosition(combine) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
