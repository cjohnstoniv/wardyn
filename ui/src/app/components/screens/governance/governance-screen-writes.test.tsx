/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — what a refused or in-flight write does to the list and its confirm. Split from
// governance-screen.test.tsx, which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
const deleteProfileMock = vi.fn();
const deleteAssignmentMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      deleteProfile: (...a: unknown[]) => deleteProfileMock(...a),
      deleteAssignment: (...a: unknown[]) => deleteAssignmentMock(...a),
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

import { HttpError } from "../../../lib/api/core";
import type { GovernanceProfile, GovernanceSnapshot } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { PERM } from "../../../lib/permissions-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GovernanceScreen } from "./governance-screen";
import { aheadByHours } from "../../../lib/test-clock";

const profile = (id: string, name: string): GovernanceProfile => ({
  id,
  name,
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
});
const GREENFIELD = profile("p1", "Greenfield contractors");
const PLATFORM = profile("p2", "Platform");
const SNAPSHOT: GovernanceSnapshot = {
  profiles: [GREENFIELD, PLATFORM],
  assignments: [{ id: "a1", subject_type: "group", subject: "wardyn.platform", profile_id: PLATFORM.id, priority: 10, created_at: aheadByHours(-1) }],
};

function renderScreen() {
  getGovernanceMock.mockResolvedValue(SNAPSHOT);
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
  deleteProfileMock.mockReset();
  deleteAssignmentMock.mockReset();
});

describe("GovernanceScreen — a refused or in-flight write", () => {
  // #2507: the refused delete re-reads, so the row stops saying "Not assigned" (the drives delete does the same).
  it("the race: a 409 re-reads the list", async () => {
    deleteProfileMock.mockRejectedValue(new HttpError(409, "this governance profile is still assigned"));
    renderScreen();
    await screen.findByText(GREENFIELD.name);
    const before = getGovernanceMock.mock.calls.length;

    await userEvent.click(screen.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: GOV.DELETE }));
    await within(dialog).findByText(GOV.DELETE_RESTRICT_TITLE);
    expect(getGovernanceMock.mock.calls.length).toBe(before + 1);
  });

  // #2508: the confirm must not take a second click while the first DELETE is in flight.
  it("the unassign confirm is disabled while the delete is in flight, so it is sent once", async () => {
    let finish!: () => void;
    deleteAssignmentMock.mockReturnValue(new Promise<void>((r) => (finish = r)));
    renderScreen();
    await screen.findByText("wardyn.platform");

    await userEvent.click(screen.getByRole("button", { name: `${PERM.REMOVE} wardyn.platform` }));
    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: PERM.REMOVE });
    await userEvent.click(confirm);
    expect(confirm).toBeDisabled();
    await userEvent.click(confirm);
    expect(deleteAssignmentMock).toHaveBeenCalledTimes(1);
    finish();
  });
});
