/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — what each section says once, and where the longer text lives (#3049, #3060). Split from
// governance-screen.test.tsx, which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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

import type { GovernanceEffective, GovernanceUnit, GovernanceSnapshot } from "../../../lib/api/governance";
import { CHANGES, GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { DEFAULT_GOVERNANCE_UNIT_ID } from "../../../lib/types/governance";
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
const WITH_ROWS: GovernanceSnapshot = {
  profiles: [PLATFORM],
  assignments: [{ id: "a1", subject_type: "group", subject: "wardyn.platform", unit_id: PLATFORM.id, priority: 10, created_at: aheadByHours(-1) }],
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
  listChangesMock.mockReset().mockResolvedValue([]);
  // The signed-in person's own ceiling, narrower than the deployment's.
  getDefaultPolicyMock.mockReset().mockResolvedValue({ allowed_domains: ["mine.corp"], min_confinement_class: "CC2" });
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

describe("GovernanceScreen — the Default unit", () => {
  it("a snapshot holding only the Default unit is the empty state: the console hides Default", async () => {
    renderScreen({ profiles: [{ ...PLATFORM, id: DEFAULT_GOVERNANCE_UNIT_ID, name: "Default", kind: "default", depth: 1 }], assignments: [] });
    expect(await screen.findByText(GOV.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.queryByText("Default")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: GOV.NEW_CTA })).toBeInTheDocument();
  });

  const DEPLOYMENT_WALLS: GovernanceEffective = { ceiling: { ...PLATFORM.ceiling, allowed_domains: ["*"] }, limits: {} };
  const DEFAULT_UNIT: GovernanceUnit = {
    ...PLATFORM, id: DEFAULT_GOVERNANCE_UNIT_ID, name: "Default", kind: "default", depth: 1, effective: DEPLOYMENT_WALLS,
  };
  const COMPOSED: GovernanceUnit = { ...PLATFORM, id: "c1", name: "Composed", overlay: {}, parent_id: DEFAULT_GOVERNANCE_UNIT_ID };

  async function openComposed() {
    await userEvent.click(await screen.findByRole("button", { name: `${GOV.EDIT} ${COMPOSED.name}` }));
    const editor = await screen.findByTestId("governance-profile-editor");
    expect(within(editor).getByText(GOV.BASE_DEPLOYMENT)).toBeInTheDocument();
    return editor;
  }

  it("a unit based on Default opens on the deployment ceiling, which is Default's, not the editor's own", async () => {
    renderScreen({ profiles: [DEFAULT_UNIT, COMPOSED], assignments: [] });
    const editor = await openComposed();
    expect(within(editor).getAllByText(GOV.OVERLAY_INHERITED("*")).length).toBeGreaterThan(0);
    expect(within(editor).queryByText(GOV.OVERLAY_INHERITED("mine.corp"))).toBeNull();
    expect(getDefaultPolicyMock).not.toHaveBeenCalled();
  });

  it("a read without Default takes the deployment ceiling from the walls a subtree hangs from", async () => {
    renderScreen({
      profiles: [COMPOSED],
      assignments: [],
      parent_walls: [{ unit_id: COMPOSED.id, parent_id: DEFAULT_GOVERNANCE_UNIT_ID, parent_name: "Default", effective: DEPLOYMENT_WALLS }],
    });
    const editor = await openComposed();
    expect(within(editor).getAllByText(GOV.OVERLAY_INHERITED("*")).length).toBeGreaterThan(0);
    expect(within(editor).queryByText(GOV.OVERLAY_INHERITED("mine.corp"))).toBeNull();
  });

  // A unit admin's read with three subtree roots: two under different units outside their scope and one under
  // Default. The walls are listed with another root's first, so each root must find its own by unit id.
  const ROOT: GovernanceUnit = { ...PLATFORM, id: "r1", name: "Root", overlay: {}, parent_id: "u-out" };
  const OTHER: GovernanceUnit = { ...ROOT, id: "r2", name: "Other", parent_id: "u-out2" };
  const walls = (domain: string, limits = {}): GovernanceEffective => ({ ceiling: { ...PLATFORM.ceiling, allowed_domains: [domain] }, limits });
  const THREE_ROOTS: GovernanceSnapshot = {
    profiles: [COMPOSED, ROOT, OTHER],
    assignments: [],
    parent_walls: [
      { unit_id: OTHER.id, parent_id: "u-out2", parent_name: "Finance", effective: walls("fin.corp") },
      { unit_id: ROOT.id, parent_id: "u-out", parent_name: "Ops", effective: walls("ops.corp", { max_concurrent_runs: 7 }) },
      { unit_id: COMPOSED.id, parent_id: DEFAULT_GOVERNANCE_UNIT_ID, parent_name: "Default", effective: DEPLOYMENT_WALLS },
    ],
  };

  it("a root under a unit outside the viewer's scope inherits the walls and limits the snapshot carries for it, not nothing", async () => {
    renderScreen(THREE_ROOTS);
    await userEvent.click(await screen.findByRole("button", { name: `${GOV.EDIT} ${ROOT.name}` }));
    const editor = await screen.findByTestId("governance-profile-editor");
    expect(within(editor).getAllByText(GOV.OVERLAY_INHERITED("ops.corp")).length).toBeGreaterThan(0);
    expect(within(editor).queryByText(GOV.OVERLAY_INHERITED("fin.corp"))).toBeNull();
    expect(within(editor).getAllByText(GOV.OVERLAY_INHERITED("7")).length).toBeGreaterThan(0);
  });

  it("a root under Default beside roots under other units still opens on Default's walls", async () => {
    renderScreen(THREE_ROOTS);
    const editor = await openComposed();
    expect(within(editor).getAllByText(GOV.OVERLAY_INHERITED("*")).length).toBeGreaterThan(0);
    expect(within(editor).queryByText(GOV.OVERLAY_INHERITED("fin.corp"))).toBeNull();
  });

  it("carries the server's cut from the list read to the tab's count and the note above the list", async () => {
    const row = {
      id: "c1", target_kind: "governance_unit", op: "update", target_key: "team-a", state: "pending" as const,
      proposed_by: "ana@example.com", proposed_at: aheadByHours(-2), expires_at: aheadByHours(20),
      diff: { before: { name: "team-a" }, after: { name: "team-a" }, changed: [] },
    };
    listChangesMock.mockResolvedValue(Object.assign([row, { ...row, id: "c2", target_key: "team-b" }], { truncated: true }));
    renderScreen();
    await userEvent.click(await screen.findByRole("tab", { name: CHANGES.TAB_COUNT_CUT(2) }));
    expect(await screen.findByText(CHANGES.TRUNCATED)).toBeInTheDocument();
  });

  it("a membership on Default shows the unit's name, not its id", async () => {
    const onDefault = { id: "a2", subject_type: "group" as const, subject: "wardyn.ops", unit_id: DEFAULT_GOVERNANCE_UNIT_ID, priority: 5, created_at: aheadByHours(-1) };
    renderScreen({ profiles: [DEFAULT_UNIT, PLATFORM], assignments: [onDefault] });
    const row = (await screen.findByText("wardyn.ops")).closest("tr") as HTMLElement;
    expect(within(row).getByText("Default")).toBeInTheDocument();
    expect(within(row).queryByText(DEFAULT_GOVERNANCE_UNIT_ID)).toBeNull();
  });
});
