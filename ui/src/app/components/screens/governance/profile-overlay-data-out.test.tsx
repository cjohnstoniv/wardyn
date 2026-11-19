/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A profile's Ceiling row for data_out: off it inherits the base's value, switched on it is one of three
// values, the effective view lists it, and a looser value than the base is reported in the server's words.
// The sentences are written out here, so a drift in copy/data-out.ts fails this suite.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../lib/api/runs", () => ({
  runs: { gradePolicy: vi.fn().mockResolvedValue({ overall_risk: "medium", risk_assessment: [] }) },
}));

const createProfileMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return { ...actual, governance: { ...actual.governance, createProfile: (...a: unknown[]) => createProfileMock(...a) } };
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

import type { GovernanceUnit } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type { RunPolicySpec } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { ProfileEditor } from "./profile-editor";
import { EffectiveView } from "./profile-overlay";

const HINT = "Content leaving runs under this profile. A profile can only hold in more than its base.";
const WARNING = "data_out: the base holds content in, and an overlay can only hold in more";
const SPEC: RunPolicySpec = { allowed_domains: ["*.corp"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" };

const row = (over: Partial<GovernanceUnit>): GovernanceUnit => ({
  id: "p",
  name: "P",
  ceiling: SPEC,
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});

const BASE = row({ id: "pb", name: "Baseline", effective: { ceiling: SPEC, limits: {} } });
const BASE_REVIEW = row({ id: "pr", name: "Held", effective: { ceiling: { ...SPEC, data_out: "review" }, limits: {} } });

function renderEditor(profiles: GovernanceUnit[]) {
  render(<ProfileEditor profile={null} profiles={profiles} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);
}
const pickBase = async (name: string) => {
  await userEvent.click(screen.getByRole("combobox", { name: GOV.FIELD_BASE }));
  await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${name}`) }));
};
const narrow = () => screen.getByRole("switch", { name: `data_out ${GOV.OVERLAY_NARROW}` });
const rowBox = () => screen.getByTestId("governance-overlay-data_out");

beforeEach(() => {
  for (const m of [createProfileMock, getDefaultPolicyMock, getSetupStatusMock]) m.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getDefaultPolicyMock.mockResolvedValue(SPEC);
  createProfileMock.mockResolvedValue({ profile: BASE, warnings: [] });
});
afterEach(cleanup);

describe("the data_out Ceiling row", { timeout: 20_000 }, () => {
  it("off: inherits the base's value, reading Allowed when the base sets none, with the hint", async () => {
    renderEditor([BASE]);
    await pickBase("Baseline");
    expect(narrow()).not.toBeChecked();
    expect(within(rowBox()).getByText("Inherited: Allowed")).toBeInTheDocument();
    expect(within(rowBox()).getByText(HINT)).toBeInTheDocument();
    expect(within(rowBox()).queryByRole("button", { name: "Review" })).toBeNull();
  });

  it("switched on: three values, starting at the inherited one, and the pick is the overlay's data_out", async () => {
    renderEditor([BASE_REVIEW]);
    fireEvent.change(screen.getByLabelText(GOV.FIELD_NAME), { target: { value: "Team A" } });
    await pickBase("Held");
    expect(within(rowBox()).getByText("Inherited: Review")).toBeInTheDocument();
    await userEvent.click(narrow());
    const box = within(rowBox());
    expect(box.getByRole("button", { name: "Allowed" })).toHaveAttribute("aria-pressed", "false");
    expect(box.getByRole("button", { name: "Review" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(box.getByRole("button", { name: "Blocked" }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(createProfileMock.mock.calls[0][0].overlay).toMatchObject({ data_out: "blocked" });
  });

  it("an unset base seeds Allowed, and the row carries no lock", async () => {
    renderEditor([BASE]);
    await pickBase("Baseline");
    await userEvent.click(narrow());
    expect(within(rowBox()).getByRole("button", { name: "Allowed" })).toHaveAttribute("aria-pressed", "true");
    expect(within(rowBox()).queryByRole("checkbox")).toBeNull();
  });
});

describe("the effective view with data_out", () => {
  const eff = (data_out?: RunPolicySpec["data_out"], warnings: string[] = []) =>
    row({ effective: { ceiling: { ...SPEC, ...(data_out ? { data_out } : {}) }, limits: {}, warnings } });

  it("lists data_out with the other ceiling fields", () => {
    render(<EffectiveView profile={eff("review")} />);
    expect(screen.getByText("data_out")).toBeInTheDocument();
    expect(screen.getByTestId("governance-effective-data_out")).toHaveTextContent("Review");
  });

  it("an unset value reads Allowed", () => {
    render(<EffectiveView profile={eff()} />);
    expect(screen.getByTestId("governance-effective-data_out")).toHaveTextContent("Allowed");
  });

  it("a profile that set a looser value than its base stays at the base's and prints the server's warning", () => {
    render(<EffectiveView profile={eff("review", [WARNING])} />);
    expect(screen.getByTestId("governance-effective-data_out")).toHaveTextContent("Review");
    expect(screen.getByText(WARNING)).toBeInTheDocument();
  });
});
