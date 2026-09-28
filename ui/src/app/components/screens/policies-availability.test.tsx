/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #923, editors.html section 1 and states.html state 8: the stored policy's
// "Available to" on its detail sheet, and New policy asking who gets it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicy } from "../../lib/types";
import { HttpError } from "../../lib/api/core";
import { aheadByHours } from "../../lib/test-clock";

const listPoliciesMock = vi.fn();
const createPolicyMock = vi.fn();
vi.mock("../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => listPoliciesMock(),
    createPolicy: (...a: unknown[]) => createPolicyMock(...a),
    updatePolicy: vi.fn(),
    deletePolicy: vi.fn(),
    getDefaultPolicy: vi.fn().mockRejectedValue(new Error("not mocked")),
  },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }) },
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
      listUserTypes: async () => [
        { id: "portfolio-manager", name: "Portfolio manager", description: "", priority: 0, built_in: false },
      ],
    },
  };
});

import { AVAILABILITY } from "../../lib/availability-copy";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { OperatorProvider } from "../wardyn/operator-context";
import { PoliciesScreen } from "./policies";

const POLICY: RunPolicy = {
  id: "pol-7f3a2c",
  name: "Read-only research",
  created_at: aheadByHours(-72),
  updated_at: aheadByHours(-2),
  spec: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC2", eligible_grants: [] },
};
const UNKNOWN_TYPE = 'The user type "portfolio-mgr" doesn\'t exist. Create it under User types first.';

beforeEach(() => {
  listPoliciesMock.mockReset();
  createPolicyMock.mockReset();
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

async function openSheet() {
  await userEvent.click(await screen.findByText(POLICY.name));
  return screen.findByRole("dialog");
}

describe("the policy sheet's Available to", () => {
  beforeEach(() => listPoliciesMock.mockResolvedValue([POLICY]));

  it("a super admin: the control after UI apps and before View raw JSON, with the policy's two lines", async () => {
    render(<PoliciesScreen />);
    const sheet = await openSheet();

    expect(await within(sheet).findByText(AVAILABILITY.LABEL)).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith("policy", POLICY.id);
    expect(within(sheet).getByTestId("availability-only").textContent).toBe(AVAILABILITY.POLICY_ONLY_HINT);
    expect(within(sheet).getByText(AVAILABILITY.POLICY_NOTE)).toBeInTheDocument();
    const label = within(sheet).getByText(AVAILABILITY.LABEL);
    const raw = within(sheet).getByText("View raw JSON");
    expect(label.compareDocumentPosition(raw) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("a security admin (state 8): the control works, and Edit policy is disabled with today's reason", async () => {
    render(
      <OperatorProvider operator={false} securityOperator={true}>
        <PoliciesScreen />
      </OperatorProvider>,
    );
    const sheet = await openSheet();

    expect(await within(sheet).findByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeEnabled();
    expect(within(sheet).getByRole("button", { name: /Edit policy/ })).toBeDisabled();
    expect(within(sheet).getByText(OPERATOR_ONLY_REASON)).toBeInTheDocument();
  });

  it("anyone else (state 8): the same sheet with no Available to at all, and nothing read", async () => {
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <PoliciesScreen />
      </OperatorProvider>,
    );
    const sheet = await openSheet();

    expect(within(sheet).getByText("View raw JSON")).toBeInTheDocument();
    expect(within(sheet).queryByText(AVAILABILITY.LABEL)).not.toBeInTheDocument();
    expect(within(sheet).queryByText(AVAILABILITY.LOAD_FAILED)).not.toBeInTheDocument();
    expect(getAvailabilityMock).not.toHaveBeenCalled();
  });
});

describe("New policy asks who gets it (decision 4)", () => {
  async function openNew() {
    listPoliciesMock.mockResolvedValue([]);
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/^name/i), "Read-only research");
    return dialog;
  }

  it("starts at Everyone; Create writes the policy, then the list, then Only these", async () => {
    const calls: string[] = [];
    createPolicyMock.mockImplementation(async (name: string) => {
      calls.push(`create ${name}`);
      return POLICY;
    });
    upsertGrantMock.mockImplementation(async (g: { subject: string; capability: string; value: string }) =>
      calls.push(`grant ${g.subject} ${g.capability} ${g.value}`),
    );
    putAvailabilityMock.mockImplementation(async (kind: string, value: string, r: boolean) =>
      calls.push(`restrict ${kind} ${value} ${r}`),
    );
    const dialog = await openNew();

    expect(within(dialog).getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    expect(within(dialog).getByTestId("availability-only").textContent).toBe(AVAILABILITY.POLICY_ONLY_HINT);
    expect(within(dialog).getByText(AVAILABILITY.POLICY_NOTE)).toBeInTheDocument();
    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    expect(await within(dialog).findByText("Portfolio manager")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    expect(upsertGrantMock).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: /create policy/i }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls).toEqual([
      "create Read-only research",
      `grant portfolio-manager policy ${POLICY.id}`,
      `restrict policy ${POLICY.id} true`,
    ]);
  });

  it("the policy saved but its list was refused: stays open on it, with the server's sentence and what holds now", async () => {
    createPolicyMock.mockResolvedValue(POLICY);
    upsertGrantMock.mockRejectedValue(new HttpError(400, UNKNOWN_TYPE));
    const dialog = await openNew();

    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-mgr");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.click(within(dialog).getByRole("button", { name: /create policy/i }));

    expect(await within(dialog).findByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(UNKNOWN_TYPE)).toBeInTheDocument();
    expect(within(dialog).getByText(AVAILABILITY.CREATE_PARTIAL_EVERYONE)).toBeInTheDocument();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    // The live control on the saved policy, and the editor is now its edit.
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalledWith("policy", POLICY.id));
    expect(await within(dialog).findByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    expect(within(dialog).getByRole("heading", { name: "Edit policy" })).toBeInTheDocument();
    expect(createPolicyMock).toHaveBeenCalledTimes(1);
  });
});
