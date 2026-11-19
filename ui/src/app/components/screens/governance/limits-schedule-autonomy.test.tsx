/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Deny scheduled runs" limit (deny_scheduled_runs) and the rubric's custom-component row
// (self_defined_component) in the profile editor: the Limits row, its read-only state, the composed overlay's row,
// the profile list's chip, and the rubric select that writes its level.
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
import { SCHEDULE_DOOR } from "../../wardyn/copy/schedule-door";
import { LIMITS_CHIP, RUBRIC } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
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

describe("the scheduled-runs limit", () => {
  it("off and on: a switch with its hint that writes deny_scheduled_runs", async () => {
    mount(row({}));
    const door = screen.getByRole("switch", { name: SCHEDULE_DOOR.LABEL });
    expect(door).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText(SCHEDULE_DOOR.HINT)).toBeInTheDocument();
    await userEvent.click(door);
    expect(door).toHaveAttribute("aria-checked", "true");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith("p", expect.objectContaining({ limits: { deny_scheduled_runs: true } }));
  });

  it("reads on when the profile already denies it", () => {
    mount(row({ limits: { deny_scheduled_runs: true } }));
    expect(screen.getByRole("switch", { name: SCHEDULE_DOOR.LABEL })).toHaveAttribute("aria-checked", "true");
  });

  it("readonly: a profile the viewer may not edit shows the row, disabled", () => {
    mount(row({ limits: { deny_scheduled_runs: true } }), true);
    expect(screen.getByRole("switch", { name: SCHEDULE_DOOR.LABEL })).toBeDisabled();
  });

  it("chip: the profile list names the limit with the chip, not the row's label", () => {
    const { unmount } = render(<>{limitChips({ deny_scheduled_runs: true })}</>);
    expect(screen.getByText(SCHEDULE_DOOR.CHIP)).toBeInTheDocument();
    expect(screen.queryByText(SCHEDULE_DOOR.LABEL)).toBeNull();
    expect(limitsBoundNothing({ deny_scheduled_runs: true })).toBe(false);
    unmount();
  });

  it("carries its label and hint in a composed profile's overlay rows", () => {
    expect(LIMIT_FIELDS.find((f) => f.key === "deny_scheduled_runs")).toMatchObject({ kind: "true", label: SCHEDULE_DOOR.LABEL, mono: false, hint: SCHEDULE_DOOR.HINT });
  });
});

describe("the self-defined component rubric row", () => {
  it("is a select in its own group that writes the level", async () => {
    mount(row({}));
    expect(screen.getByText(RUBRIC.GROUP_COMPONENTS)).toBeInTheDocument();
    const [label] = RUBRIC.ROWS.self_defined_component;
    await userEvent.click(screen.getByRole("combobox", { name: `${label} caps autonomy at` }));
    await userEvent.click(await screen.findByRole("option", { name: AUTONOMY_META.L1.label }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith(
      "p",
      expect.objectContaining({ limits: { autonomy_rubric: { self_defined_component: "L1" } } }),
    );
  });

  it("counts toward the profile list chip's strictest cap", () => {
    const { unmount } = render(<>{limitChips({ autonomy_rubric: { self_defined_component: "L0" } })}</>);
    expect(screen.getByText(LIMITS_CHIP.AUTONOMY(AUTONOMY_META.L0.label))).toBeInTheDocument();
    unmount();
  });
});
