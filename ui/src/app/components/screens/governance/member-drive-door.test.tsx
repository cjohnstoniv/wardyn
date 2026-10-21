/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Can't create drives" door (deny_member_drives) in the profile editor: the
// standalone Limits row, the composed overlay's row, and the chip that names it
// in the Governance list.
import { render, screen, within } from "@testing-library/react";
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
import { GOVERNANCE as GOV, MEMBER_DRIVE_DOOR } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { setup } from "../../../lib/api/setup";
import { aheadByHours } from "../../../lib/test-clock";
import { limitChips } from "./limit-chips";
import { LIMIT_FIELDS } from "./profile-overlay";
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

const mount = (profile: GovernanceUnit, profiles: GovernanceUnit[] = [profile]) =>
  render(<ProfileEditor profile={profile} profiles={profiles} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);

describe("the member-drive door", () => {
  it("is a switch in the standalone Limits section that writes deny_member_drives", async () => {
    mount(row({}));
    const door = screen.getByRole("switch", { name: MEMBER_DRIVE_DOOR.LABEL });
    expect(door).toHaveAttribute("aria-checked", "false");
    await userEvent.click(door);
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith("p", expect.objectContaining({ limits: { deny_member_drives: true } }));
  });

  it("reads on when the profile already denies it, and the user-drive door beside it stays off", () => {
    mount(row({ limits: { deny_member_drives: true } }));
    expect(screen.getByRole("switch", { name: MEMBER_DRIVE_DOOR.LABEL })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("switch", { name: GOV.LIMIT_DRIVE_LABEL })).toHaveAttribute("aria-checked", "false");
  });

  it("carries its friendly label in a composed profile's overlay rows, not the wire key", () => {
    const field = LIMIT_FIELDS.find((f) => f.key === "deny_member_drives");
    expect(field).toMatchObject({ kind: "true", label: MEMBER_DRIVE_DOOR.LABEL, mono: false });
  });

  it("is named by a chip when set, and absent when not", () => {
    const { unmount } = render(<>{limitChips({ deny_member_drives: true })}</>);
    expect(screen.getByText(MEMBER_DRIVE_DOOR.LABEL)).toBeInTheDocument();
    unmount();
    const { container } = render(<>{limitChips({})}</>);
    expect(within(container).queryByText(MEMBER_DRIVE_DOOR.LABEL)).toBeNull();
  });
});
