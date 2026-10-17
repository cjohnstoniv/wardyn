/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicy } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";

// go-live findings pinned here:
//  - ui-secretsPolicies-1: table row must keep its "row" a11y role (no
//    role="button" stripping table structure) while staying keyboard-operable.
//  - ui-secretsPolicies-3: required Name/Spec fields must be marked required.
//  - ui-secretsPolicies-5: the toolbar (search/count/refresh) must gate on
//    status==="ready" && non-empty, and the count must show "X of Y".

const listPoliciesMock = vi.fn();
const createPolicyMock = vi.fn();
const updatePolicyMock = vi.fn();
const getDefaultPolicyMock = vi.fn<() => Promise<unknown>>(() => Promise.reject(new Error("not mocked")));
vi.mock("../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => listPoliciesMock(),
    createPolicy: (...a: unknown[]) => createPolicyMock(...a),
    updatePolicy: (...a: unknown[]) => updatePolicyMock(...a),
    deletePolicy: vi.fn(),
    getDefaultPolicy: () => getDefaultPolicyMock(),
  },
}));

// The create/edit editor's PolicyPanel renders the SafetyMeter, which debounces
// a POST /policies/grade; stub it so the editor tests never touch the network.
vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }) },
}));

// New policy asks the setup status which model providers the deployment has; a
// rejection (the default here) leaves the editor on the stock starter.
const getSetupStatusMock = vi.fn<() => Promise<unknown>>(() => Promise.reject(new Error("not mocked")));
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: () => getSetupStatusMock() },
}));

import { PoliciesScreen } from "./policies";
import { toYaml } from "../wardyn/yaml-block";

function policy(over: Partial<RunPolicy> = {}): RunPolicy {
  return {
    id: "pol-1",
    name: "payments-strict",
    created_at: aheadByHours(-1),
    updated_at: aheadByHours(-1),
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

// F5-F9: the editor discarded a hand-written spec on Escape/overlay click,
// no draft, no confirm — the in-repo precedent (commit a91b3529, "the wizard
// is now the one edit surface, and can't be dismissed by accident") blocks
// dismissal outright rather than confirming: onPointerDownOutside/
// onEscapeKeyDown preventDefault while dirty, scoped to this dialog only.
describe("PoliciesScreen — the policy editor can't be dismissed by accident once dirty", () => {
  // ticket: F5-F9
  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([]);
    createPolicyMock.mockReset();
  });

  it("blocks Escape once the Name field has been edited", async () => {
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));
    await userEvent.type(screen.getByLabelText(/^name/i), "my-policy");

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByLabelText(/^name/i)).toHaveValue("my-policy");
  });

  it("neg: an untouched editor still closes on Escape", async () => {
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("PoliciesScreen: New policy follows the deployment's model providers", () => {
  const BEDROCK_HOST = "bedrock-runtime.us-east-1.amazonaws.com";

  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([]);
    getSetupStatusMock.mockReset();
    getSetupStatusMock.mockResolvedValue({
      model_providers: [{ id: "p1", kind: "bedrock_sso", host: BEDROCK_HOST, harnesses: ["claude-code"] }],
    });
  });

  afterEach(() => getSetupStatusMock.mockReset());

  it("re-seeds an untouched spec onto the Bedrock host and leaves the editor clean", async () => {
    render(<PoliciesScreen />);
    await screen.findByText(/no policies yet/i);
    await userEvent.click(screen.getByRole("button", { name: /new policy/i }));
    await screen.findByText(/Model hosts follow/);

    const spec = screen.getByRole("dialog").querySelector("textarea") as HTMLTextAreaElement;
    await waitFor(() => expect(spec.value).toContain(BEDROCK_HOST));
    expect(spec.value).not.toContain("api.anthropic.com");

    // Untouched means not dirty: Escape closes the dialog.
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("a slow /setup/status that settles after New policy was closed never rewrites a later Edit", async () => {
    let settle!: (v: unknown) => void;
    getSetupStatusMock.mockReset();
    getSetupStatusMock.mockReturnValueOnce(new Promise((r) => (settle = r)));
    getSetupStatusMock.mockResolvedValue({ model_providers: [] });
    listPoliciesMock.mockResolvedValue([policy()]);
    render(<PoliciesScreen />);
    await screen.findByText("payments-strict");
    await userEvent.click(screen.getAllByRole("button", { name: /new policy/i })[0]);
    await screen.findByRole("dialog");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /policy actions/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /^edit/i }));
    const spec = (await screen.findByRole("dialog")).querySelector("textarea") as HTMLTextAreaElement;
    expect(spec.value).toContain("deny_with_review");

    settle({ model_providers: [{ id: "p1", kind: "bedrock_sso", host: BEDROCK_HOST, harnesses: ["claude-code"] }] });
    await new Promise((r) => setTimeout(r, 20));
    expect(spec.value).toContain("deny_with_review");
    expect(spec.value).not.toContain(BEDROCK_HOST);
  });
});

// #1921: one read-only policy document, and a source editor that opens in YAML.
describe("PoliciesScreen — reading a policy never writes it", () => {
  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([policy()]);
    createPolicyMock.mockReset();
    updatePolicyMock.mockReset();
    getDefaultPolicyMock.mockReset();
    getDefaultPolicyMock.mockResolvedValue({ allowed_domains: ["mirror.example"], min_confinement_class: "CC2" });
  });

  it("the selected policy and the default open on Summary, switch views, and cause no policy write", async () => {
    const user = userEvent.setup();
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByText("payments-strict"));
    const sheet = await screen.findByRole("dialog");
    fireEvent.click(within(sheet).getByText("View raw JSON"));
    expect(within(sheet).getByRole("button", { name: "Summary" })).toHaveAttribute("aria-pressed", "true");
    expect(within(sheet).getByText("Allowed hosts").nextElementSibling).toHaveTextContent("api.anthropic.com");
    for (const name of ["YAML", "JSON", "Summary"]) await user.click(within(sheet).getByRole("button", { name }));
    fireEvent.keyDown(sheet, { key: "Escape", code: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    fireEvent.click(await screen.findByText(/^Default \(ceiling\) policy/));
    expect(await screen.findByText("mirror.example")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "YAML" }));

    expect(updatePolicyMock).not.toHaveBeenCalled();
    expect(createPolicyMock).not.toHaveBeenCalled();
  });
});

describe("PoliciesScreen — the editor opens in YAML and saves what it parses to", () => {
  beforeEach(() => {
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([policy()]);
    createPolicyMock.mockReset();
    updatePolicyMock.mockReset();
    updatePolicyMock.mockResolvedValue(policy());
    getDefaultPolicyMock.mockReset();
    getDefaultPolicyMock.mockRejectedValue(new Error("not mocked"));
  });

  const openEdit = async () => {
    render(<PoliciesScreen />);
    await screen.findByText("payments-strict");
    await userEvent.click(screen.getByRole("button", { name: /policy actions/i }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /^edit/i }));
    return (await screen.findByLabelText(/^Spec \(YAML\)/)) as HTMLTextAreaElement;
  };

  it("seeds the stored policy as YAML and sends the parsed object, not the text", async () => {
    const box = await openEdit();
    expect(box.value).toBe(`${toYaml(policy().spec)}\n`);
    fireEvent.change(box, { target: { value: "# tightened\nallowed_domains: []\nmin_confinement_class: CC3\n" } });
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(updatePolicyMock).toHaveBeenCalledTimes(1));
    expect(updatePolicyMock).toHaveBeenCalledWith("pol-1", "payments-strict", {
      allowed_domains: [],
      min_confinement_class: "CC3",
    });
  });

  it("an invalid source cannot be saved: nothing is sent, least of all the last valid policy", async () => {
    const box = await openEdit();
    fireEvent.change(box, { target: { value: "a: [" } });
    expect(screen.getByText(/^Invalid YAML — /)).toBeInTheDocument();
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();
    fireEvent.click(save);
    expect(updatePolicyMock).not.toHaveBeenCalled();
  });
});
