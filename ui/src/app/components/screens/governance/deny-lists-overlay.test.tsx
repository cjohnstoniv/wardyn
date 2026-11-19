/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composed profile's deny-list rows: Narrow starts from an empty box that only adds, the composed hints and
// placeholder, and the heading a bad list is refused under. The new strings are written out, not read from the
// copy module, so a drift in copy/deny-lists.ts fails this suite.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../lib/api/runs", () => ({
  runs: { gradePolicy: vi.fn().mockResolvedValue({ overall_risk: "medium", risk_assessment: [] }) },
}));

const createProfileMock = vi.fn();
const updateProfileMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      createProfile: (...a: unknown[]) => createProfileMock(...a),
      updateProfile: (...a: unknown[]) => updateProfileMock(...a),
    },
  };
});

const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a) },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

vi.mock("../../../lib/api/components", () => ({ components: { list: vi.fn().mockResolvedValue([]) } }));
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: vi.fn().mockResolvedValue({ providers: { providers: [] }, connected: {}, etag: null }) },
}));

import { HttpError } from "../../../lib/api/core";
import type { GovernanceUnit } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type { RunPolicySpec } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { DENY_LISTS } from "../../wardyn/copy/deny-lists";
import { composeRefusalTitle } from "./profile-overlay";
import { ProfileEditor } from "./profile-editor";

const SPEC: RunPolicySpec = { allowed_domains: ["*.corp"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" };
const COMP_A = "3f6c9a1e-5b7d-4c21-9e0a-7d2b8c4f1a60";
const COMP_B = "8a21d4c7-0e93-4b5f-a1c6-2f7e9b3d5c18";
const PROV_A = "5d1e7b90-2c4a-4f68-8b13-a9c0e6d27f41";

const row = (over: Partial<GovernanceUnit>): GovernanceUnit => ({
  id: "p",
  name: "P",
  ceiling: SPEC,
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});

const BASE = row({
  id: "pb",
  name: "Baseline",
  effective: { ceiling: { ...SPEC, allowed_methods: ["GET", "POST"] }, limits: { deny_components: [COMP_A, COMP_B], deny_model_providers: [PROV_A] } },
});

const COMPONENTS_HINT_COMPOSED =
  "Component ids people on this profile may not attach to a run. A run that attaches one is refused, and a queued run that does ends when it starts. Applies to stored components only. Adds to what the base denies. Entries the base denies stay denied.";
const PROVIDERS_HINT_COMPOSED =
  "Model provider uids people on this profile may not use. A run that would use one is refused, and a queued run that would ends when it starts. Adds to what the base denies. Entries the base denies stay denied.";

function renderEditor(profile: GovernanceUnit | null, profiles: GovernanceUnit[] = [BASE]) {
  render(<ProfileEditor profile={profile} profiles={profiles} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);
}

const pickBase = async (name: string) => {
  await userEvent.click(screen.getByRole("combobox", { name: GOV.FIELD_BASE }));
  await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${name}`) }));
};
const narrow = (label: string) => screen.getByRole("switch", { name: `${label} ${GOV.OVERLAY_NARROW}` });

beforeEach(() => {
  for (const m of [createProfileMock, updateProfileMock, getDefaultPolicyMock, getSetupStatusMock]) m.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getDefaultPolicyMock.mockResolvedValue({ ...SPEC, allowed_domains: ["*"], allowed_methods: ["GET", "POST"] });
  createProfileMock.mockResolvedValue({ profile: BASE, warnings: [] });
  updateProfileMock.mockResolvedValue({ profile: BASE, warnings: [] });
});
afterEach(cleanup);

describe("composed deny-list rows", { timeout: 20_000 }, () => {
  it("Narrow starts the box empty, with the base's ids on the Inherited line", async () => {
    renderEditor(null);
    fireEvent.change(screen.getByLabelText(GOV.FIELD_NAME), { target: { value: "Team A" } });
    await pickBase("Baseline");
    await userEvent.click(narrow(DENY_LISTS.COMPONENTS_LABEL));
    await userEvent.click(narrow(DENY_LISTS.PROVIDERS_LABEL));
    expect(screen.getByLabelText(DENY_LISTS.COMPONENTS_LABEL)).toHaveValue("");
    expect(screen.getByLabelText(DENY_LISTS.PROVIDERS_LABEL)).toHaveValue("");
    expect(screen.getByText(GOV.OVERLAY_INHERITED(`${COMP_A}, ${COMP_B}`))).toBeInTheDocument();
    expect(screen.getByText(GOV.OVERLAY_INHERITED(PROV_A))).toBeInTheDocument();
  });

  it("an added id is saved as the overlay's own list, and an untouched row sends nothing", async () => {
    renderEditor(null);
    fireEvent.change(screen.getByLabelText(GOV.FIELD_NAME), { target: { value: "Team A" } });
    await pickBase("Baseline");
    await userEvent.click(narrow(DENY_LISTS.COMPONENTS_LABEL));
    fireEvent.change(screen.getByLabelText(DENY_LISTS.COMPONENTS_LABEL), { target: { value: "7b0e2d54-9c13-4a8f-b6e1-4d5a3f9c2e07" } });
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(createProfileMock.mock.calls[0][0].overlay_limits).toEqual({ deny_components: ["7b0e2d54-9c13-4a8f-b6e1-4d5a3f9c2e07"] });
  });

  it("another list row still seeds what the base holds: Narrow on allowed_methods starts from GET", async () => {
    renderEditor(null);
    fireEvent.change(screen.getByLabelText(GOV.FIELD_NAME), { target: { value: "Team A" } });
    await pickBase("Baseline");
    await userEvent.click(narrow("allowed_methods"));
    expect(screen.getByLabelText("allowed_methods")).toHaveValue("GET\nPOST");
  });

  it("shows the composed hints and the placeholder", async () => {
    renderEditor(null);
    await pickBase("Baseline");
    expect(screen.getByText(COMPONENTS_HINT_COMPOSED)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS_HINT_COMPOSED)).toBeInTheDocument();
    await userEvent.click(narrow(DENY_LISTS.COMPONENTS_LABEL));
    expect(screen.getByLabelText(DENY_LISTS.COMPONENTS_LABEL)).toHaveAttribute("placeholder", "One id per line");
  });

  it("a bad list is refused under 'This list can't be saved', with the server's sentence under it", async () => {
    const sentence = 'governance_overlay_invalid: deny_components: entry 1 ("not-an-id") is not an id — use the lower-case id from the component or provider';
    createProfileMock.mockRejectedValue(new HttpError(400, sentence, "governance_overlay_invalid"));
    renderEditor(null);
    fireEvent.change(screen.getByLabelText(GOV.FIELD_NAME), { target: { value: "Team A" } });
    await pickBase("Baseline");
    await userEvent.click(narrow(DENY_LISTS.COMPONENTS_LABEL));
    fireEvent.change(screen.getByLabelText(DENY_LISTS.COMPONENTS_LABEL), { target: { value: "not-an-id" } });
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("This list can't be saved")).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
    expect(within(alert).queryByText(GOV.REFUSED_OVERLAY_INVALID)).toBeNull();
  });
});

describe("composeRefusalTitle", () => {
  it.each([
    ["governance_overlay_invalid", "governance_overlay_invalid: deny_components: entry 1 is not an id"],
    ["governance_overlay_invalid", "governance_overlay_invalid: deny_model_providers: entry 2 is listed twice"],
    ["governance_unit_request_invalid", "limits.deny_components: entry 1 is not an id"],
    ["governance_unit_request_invalid", "limits.deny_model_providers: at most 200 entries"],
  ])("%s with %j heads the list refusal", (reason, message) => {
    expect(composeRefusalTitle(reason, message)).toBe("This list can't be saved");
  });

  it("keeps today's titles for every other sentence", () => {
    expect(composeRefusalTitle("governance_overlay_invalid", "governance_overlay_invalid: allowed_domains: widens the base")).toBe(GOV.REFUSED_OVERLAY_INVALID);
    expect(composeRefusalTitle("governance_overlay_invalid")).toBe(GOV.REFUSED_OVERLAY_INVALID);
    expect(composeRefusalTitle("governance_unit_request_invalid", "limits.max_cpu_millis: too big")).toBeUndefined();
    expect(composeRefusalTitle("governance_unit_cycle", "deny_components: x")).toBe(GOV.REFUSED_CYCLE);
    expect(composeRefusalTitle("governance_unit_depth")).toBe(GOV.REFUSED_DEPTH);
    expect(composeRefusalTitle("governance_overlay_unsatisfiable")).toBe(GOV.REFUSED_UNSATISFIABLE);
    expect(composeRefusalTitle("something_else")).toBeUndefined();
  });
});
