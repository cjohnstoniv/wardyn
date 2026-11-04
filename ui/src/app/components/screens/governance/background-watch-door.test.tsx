/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Deny watching background desktops" limit (deny_background_desktop_watch) in the profile editor: the
// standalone Limits row, right after Deny UI apps, and the composed overlay's row. Its chip and its place in
// limitsBoundNothing are limit-chips.test.tsx's.
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
import { BACKGROUND_WATCH_DOOR, GOVERNANCE as GOV, LIMITS_CHIP } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { setup } from "../../../lib/api/setup";
import { aheadByHours } from "../../../lib/test-clock";
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

describe("the background-watch limit", () => {
  it("is a switch with its hint that writes deny_background_desktop_watch", async () => {
    mount(row({}));
    const door = screen.getByRole("switch", { name: BACKGROUND_WATCH_DOOR.LABEL });
    expect(door).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText(BACKGROUND_WATCH_DOOR.HINT)).toBeInTheDocument();
    await userEvent.click(door);
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith("p", expect.objectContaining({ limits: { deny_background_desktop_watch: true } }));
  });

  it("reads on when the profile already denies it", () => {
    mount(row({ limits: { deny_background_desktop_watch: true } }));
    expect(screen.getByRole("switch", { name: BACKGROUND_WATCH_DOOR.LABEL })).toHaveAttribute("aria-checked", "true");
  });

  it("comes right after Deny UI apps, and carries its label in a composed profile's overlay rows", () => {
    mount(row({}));
    const names = screen.getAllByRole("switch").map((s) => s.getAttribute("aria-label"));
    expect(names.indexOf(BACKGROUND_WATCH_DOOR.LABEL)).toBe(names.indexOf(LIMITS_CHIP.UI_APPS) + 1);
    expect(LIMIT_FIELDS.find((f) => f.key === "deny_background_desktop_watch")).toMatchObject({
      kind: "true",
      label: BACKGROUND_WATCH_DOOR.LABEL,
      mono: false,
    });
  });
});
