/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicy } from "../../lib/types";

// go-live findings pinned here:
//  - ui-secretsPolicies-1: table row must keep its "row" a11y role (no
//    role="button" stripping table structure) while staying keyboard-operable.
//  - ui-secretsPolicies-3: required Name/Spec fields must be marked required.
//  - ui-secretsPolicies-5: the toolbar (search/count/refresh) must gate on
//    status==="ready" && non-empty, and the count must show "X of Y".

const listPoliciesMock = vi.fn();
const createPolicyMock = vi.fn();
vi.mock("../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => listPoliciesMock(),
    createPolicy: (...a: unknown[]) => createPolicyMock(...a),
    updatePolicy: vi.fn(),
    deletePolicy: vi.fn(),
  },
}));

// The create/edit editor's PolicyPanel renders the SafetyMeter, which debounces
// a POST /policies/grade; stub it so the editor tests never touch the network.
vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }) },
}));

import { PoliciesScreen } from "./policies";

function policy(over: Partial<RunPolicy> = {}): RunPolicy {
  return {
    id: "pol-1",
    name: "payments-strict",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    spec: {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
      eligible_grants: [],
    },
    ...over,
  };
}

describe("PoliciesScreen — table row semantics (ui-secretsPolicies-1)", () => {
  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([policy()]);
  });

  it("keeps the row's implicit 'row' role instead of overriding it to 'button'", async () => {
    render(<PoliciesScreen />);
    const cell = await screen.findByText("payments-strict");
    const row = cell.closest("tr")!;

    // role="button" on a <tr> strips it from the accessibility tree's table
    // structure; a plain row must still expose role="row" via getByRole.
    expect(row).not.toHaveAttribute("role", "button");
    expect(screen.getByRole("row", { name: /payments-strict/i })).toBe(row);
  });

  it("stays keyboard-operable (tabIndex + Enter opens the detail sheet)", async () => {
    render(<PoliciesScreen />);
    const cell = await screen.findByText("payments-strict");
    const row = cell.closest("tr")!;
    expect(row).toHaveAttribute("tabindex", "0");

    fireEvent.keyDown(row, { key: "Enter" });
    expect(await screen.findByRole("heading", { name: "payments-strict" })).toBeInTheDocument();
  });
});

describe("PoliciesScreen — toolbar gating + count (ui-secretsPolicies-5)", () => {
  beforeEach(() => {
    listPoliciesMock.mockReset();
  });

  it("hides the search/count/refresh toolbar while loading", () => {
    listPoliciesMock.mockReturnValue(new Promise(() => {})); // never resolves
    render(<PoliciesScreen />);
    expect(screen.queryByPlaceholderText(/search policies/i)).not.toBeInTheDocument();
  });

  it("hides the toolbar (and its duplicate Refresh) on error, leaving only ErrorState's retry", async () => {
    listPoliciesMock.mockRejectedValue(new Error("boom"));
    render(<PoliciesScreen />);
    await screen.findByRole("button", { name: /retry/i });
    expect(screen.queryByPlaceholderText(/search policies/i)).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /refresh|retry/i })).toHaveLength(1);
  });

  it("shows the toolbar with an 'X of Y policies' total once ready", async () => {
    listPoliciesMock.mockResolvedValue([policy({ id: "a", name: "one" }), policy({ id: "b", name: "two" })]);
    render(<PoliciesScreen />);
    await screen.findByText("one");
    expect(screen.getByText("2 of 2 policies")).toBeInTheDocument();

    await userEvent.type(screen.getByPlaceholderText(/search policies/i), "one");
    await waitFor(() => expect(screen.getByText("1 of 2 policies")).toBeInTheDocument());
  });
});

describe("PoliciesScreen — required fields + error announcement (ui-secretsPolicies-2/3)", () => {
  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([]);
    createPolicyMock.mockReset();
  });

  it("marks Name and Spec as required in the create dialog", async () => {
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));

    expect(screen.getByLabelText(/^name/i)).toBeRequired();
    expect(screen.getByLabelText(/^spec/i)).toBeRequired();
  });

  it("announces a rejected save via role=alert and wires it to the Save button", async () => {
    createPolicyMock.mockRejectedValue(new Error("HTTP 400: invalid spec"));
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));

    await userEvent.type(screen.getByLabelText(/^name/i), "my-policy");
    const saveBtn = screen.getByRole("button", { name: /create policy/i });
    await userEvent.click(saveBtn);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/invalid spec/i);
    expect(saveBtn).toHaveAttribute("aria-describedby", alert.id);
  });
});
