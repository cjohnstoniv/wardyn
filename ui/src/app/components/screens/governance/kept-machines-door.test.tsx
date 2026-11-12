/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-6: the "Deny kept machines" limit (deny_kept_machines) in the profile editor: the standalone Limits row, its
// read-only state, the composed overlay's row, and the profile list's chip.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

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

vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: vi.fn().mockResolvedValue({}) },
}));

vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: vi.fn() },
}));

import type { GovernanceUnit } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { KEEP_POLICY } from "../../wardyn/copy/keep-machine";
import { baseStatus } from "../../../lib/test-fixtures";
import { setup } from "../../../lib/api/setup";
import { aheadByHours } from "../../../lib/test-clock";
import { LIMIT_FIELDS } from "./profile-overlay";
import { limitChips, limitsBoundNothing } from "./limit-chips";
import { ProfileEditor } from "./profile-editor";

const row = (over: Partial<GovernanceUnit>): GovernanceUnit => ({
  id: "p",
  name: "P",
  ceiling: { allowed_domains: ["git.x"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});

beforeEach(() => {
  createProfileMock.mockReset();
  updateProfileMock.mockReset();
  updateProfileMock.mockResolvedValue({ profile: row({}), warnings: [] });
  vi.mocked(setup.getSetupStatus).mockResolvedValue(baseStatus());
});

const mount = (profile: GovernanceUnit, disabled = false, profiles: GovernanceUnit[] = [profile]) =>
  render(<ProfileEditor profile={profile} profiles={profiles} disabled={disabled} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);

describe("the kept-machines limit", () => {
  it("km6-off and km6-on: a switch with its hint that writes deny_kept_machines", async () => {
    mount(row({}));
    const door = screen.getByRole("switch", { name: KEEP_POLICY.LABEL });
    expect(door).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText(KEEP_POLICY.HINT)).toBeInTheDocument();
    await userEvent.click(door);
    expect(door).toHaveAttribute("aria-checked", "true");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith("p", expect.objectContaining({ limits: { deny_kept_machines: true } }));
  });

  it("reads on when the profile already denies it", () => {
    mount(row({ limits: { deny_kept_machines: true } }));
    expect(screen.getByRole("switch", { name: KEEP_POLICY.LABEL })).toHaveAttribute("aria-checked", "true");
  });

  it("km6-readonly: a profile the viewer may not edit shows the row, disabled", () => {
    mount(row({ limits: { deny_kept_machines: true } }), true);
    expect(screen.getByRole("switch", { name: KEEP_POLICY.LABEL })).toBeDisabled();
  });

  it("km6-chip: the profile list names the limit with the chip, not the row's label", () => {
    const { unmount } = render(<>{limitChips({ deny_kept_machines: true })}</>);
    expect(screen.getByText(KEEP_POLICY.CHIP)).toBeInTheDocument();
    expect(screen.queryByText(KEEP_POLICY.LABEL)).toBeNull();
    expect(limitsBoundNothing({ deny_kept_machines: true })).toBe(false);
    unmount();
  });

  it("carries its label and hint in a composed profile's overlay rows", () => {
    expect(LIMIT_FIELDS.find((f) => f.key === "deny_kept_machines")).toMatchObject({ kind: "true", label: KEEP_POLICY.LABEL, mono: false, hint: KEEP_POLICY.HINT });
  });
});
