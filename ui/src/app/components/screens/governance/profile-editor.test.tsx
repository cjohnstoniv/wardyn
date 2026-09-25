/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ProfileEditor standalone (0.7.2 U2) — the three integer LimitNumberRows
// (max_concurrent_runs, MaxEphemeralDiskMiB, MaxDriveSizeMiB) that a Switch
// (LimitRow) can't carry. governance-screen.test.tsx already pins the
// boolean LimitRows and the save/cancel/error plumbing through the whole
// screen; this file mounts the editor directly so the number rows don't need
// a profiles table around them.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

// SafetyMeter (inside PolicyPanel) debounces a POST /policies/grade — stub it
// so no test touches the network (governance-screen.test.tsx's precedent).
const gradePolicyMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { gradePolicy: (...a: unknown[]) => gradePolicyMock(...a) },
}));

const createProfileMock = vi.fn();
const updateProfileMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: { ...actual.governance, createProfile: (...a: unknown[]) => createProfileMock(...a), updateProfile: (...a: unknown[]) => updateProfileMock(...a) },
  };
});

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

import type { GovernanceProfile } from "../../../lib/api/governance";
import { GOVERNANCE as GOV, RUBRIC, RUN_LIMITS as RL } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import { ProfileEditor } from "./profile-editor";
import { aheadByHours } from "../../../lib/test-clock";

const GREENFIELD: GovernanceProfile = {
  id: "p1",
  name: "Greenfield contractors",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};

function renderEditor(profile: GovernanceProfile | null = GREENFIELD) {
  const onSaved = vi.fn();
  render(<ProfileEditor profile={profile} disabled={false} onCancel={vi.fn()} onSaved={onSaved} />);
  return { onSaved };
}

describe("ProfileEditor — the three integer LimitNumberRows", () => {
  beforeEach(() => {
    getSetupStatusMock.mockReset();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    gradePolicyMock.mockReset();
    gradePolicyMock.mockResolvedValue({ overall_risk: "medium", risk_assessment: [] });
  });

  it("renders all three rows with label, hint and the leave-blank-for-unlimited wording", async () => {
    renderEditor();

    expect(screen.getByText(GOV.LIMIT_CONCURRENT_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_CONCURRENT_HINT)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_EPHEMERAL_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_EPHEMERAL_HINT)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_DRIVE_SIZE_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_DRIVE_SIZE_HINT)).toBeInTheDocument();

    // F4-F8 (Appendix A V8, reclassified Low copy): numberField renders 0 as
    // an EMPTY field, so "0 means…" named a value the control can't display.
    // Every hint states the same corrected zero rule — one across all three,
    // not three different sentences a reader has to reconcile.
    for (const hint of [GOV.LIMIT_CONCURRENT_HINT, GOV.LIMIT_EPHEMERAL_HINT, GOV.LIMIT_DRIVE_SIZE_HINT]) {
      expect(hint).toMatch(/leave blank for no limit/i);
    }
  });

  it("a profile with no limits renders every number row blank — 0/absent is unlimited", () => {
    renderEditor({ ...GREENFIELD, limits: {} });
    expect(screen.getByLabelText(GOV.LIMIT_CONCURRENT_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(GOV.LIMIT_EPHEMERAL_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(GOV.LIMIT_DRIVE_SIZE_LABEL)).toHaveValue(null);
  });

  it("a saved limit shows in its own row, and editing writes the field on save", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderEditor({ ...GREENFIELD, limits: { max_concurrent_runs: 5 } });

    expect(screen.getByLabelText(GOV.LIMIT_CONCURRENT_LABEL)).toHaveValue(5);

    const ephemeral = screen.getByLabelText(GOV.LIMIT_EPHEMERAL_LABEL);
    await userEvent.clear(ephemeral);
    await userEvent.type(ephemeral, "4096");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(updateProfileMock).toHaveBeenCalledWith(
      GREENFIELD.id,
      expect.objectContaining({ limits: { max_concurrent_runs: 5, max_ephemeral_disk_mib: 4096 } }),
    );
  });

  it("shows the Docker uncapped warning under the ephemeral row only, and only for `none`", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"], ephemeral_disk_enforcement: "none" } }));
    renderEditor();

    const warning = await screen.findByText(PROVIDERS.DOCKER_UNCAPPED_WARN);
    expect(warning).toBeInTheDocument();

    // It sits under the ephemeral row, not the concurrent or drive-size rows.
    expect(within(screen.getByTestId("governance-limit-ephemeral")).getByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).toBeInTheDocument();
    expect(within(screen.getByTestId("governance-limit-concurrent")).queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
    expect(within(screen.getByTestId("governance-limit-drive-size")).queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("shows no Docker warning when the driver can enforce (filesystem)", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"], ephemeral_disk_enforcement: "filesystem" } }));
    renderEditor();

    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("shows no Docker warning on Kubernetes (eviction) — the pod binds the size", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "kubernetes", confinement_classes: ["CC2"], ephemeral_disk_enforcement: "eviction" } }));
    renderEditor();

    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("shows no Docker warning when the daemon reports no enforcement word at all", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"] } }));
    renderEditor();

    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });
});

// RL-14 (0.8, #579) — the seven run-limit rows (long-holds-design.md rev 4
// §2.2). Five carry a duration (seconds on the wire, days/hours/minutes in
// the control) and two are switches sharing LimitRow with the doors above.
describe("ProfileEditor — the seven run-limit rows", () => {
  const unitPicker = (label: string) => screen.getByRole("combobox", { name: RL.UNIT_PICKER_LABEL(label) });

  beforeEach(() => {
    getSetupStatusMock.mockReset();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    gradePolicyMock.mockReset();
    gradePolicyMock.mockResolvedValue({ overall_risk: "medium", risk_assessment: [] });
  });

  it("renders every label and hint", () => {
    renderEditor();

    expect(screen.getByText(RL.SECTION_TITLE)).toBeInTheDocument();
    expect(screen.getByText(RL.MAX_END_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.MAX_END_HINT)).toBeInTheDocument();
    expect(screen.getByText(RL.DEFAULT_END_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.ALLOW_NO_END_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.ALLOW_NO_END_HINT)).toBeInTheDocument();
    expect(screen.getByText(RL.MAX_WAIT_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.MAX_WAIT_HINT)).toBeInTheDocument();
    expect(screen.getByText(RL.DEFAULT_WAIT_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.USER_CHANGES_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.USER_CHANGES_HINT)).toBeInTheDocument();
    expect(screen.getByText(RL.PAUSE_IDLE_LABEL)).toBeInTheDocument();
    expect(screen.getByText(RL.PAUSE_IDLE_HINT)).toBeInTheDocument();
  });

  it("a profile with no run limits renders every duration row blank and every switch off", () => {
    renderEditor({ ...GREENFIELD, limits: {} });

    expect(screen.getByLabelText(RL.MAX_END_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(RL.DEFAULT_END_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(RL.MAX_WAIT_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(RL.DEFAULT_WAIT_LABEL)).toHaveValue(null);
    expect(screen.getByLabelText(RL.PAUSE_IDLE_LABEL)).toHaveValue(null);
    expect(screen.getByRole("switch", { name: RL.ALLOW_NO_END_LABEL })).not.toBeChecked();
    expect(screen.getByRole("switch", { name: RL.USER_CHANGES_LABEL })).not.toBeChecked();
  });

  it("a saved run limit shows converted to its display unit — days, hours, minutes", () => {
    renderEditor({
      ...GREENFIELD,
      limits: {
        max_end_ahead_sec: 30 * 86400,
        default_end_sec: 86400,
        max_wait_sec: 8 * 3600,
        default_wait_sec: 4 * 3600,
        pause_idle_after_sec: 30 * 60,
        allow_no_end: true,
        user_changes_limits: true,
      },
    });

    expect(screen.getByLabelText(RL.MAX_END_LABEL)).toHaveValue(30);
    expect(screen.getByLabelText(RL.DEFAULT_END_LABEL)).toHaveValue(1);
    expect(screen.getByLabelText(RL.MAX_WAIT_LABEL)).toHaveValue(8);
    expect(screen.getByLabelText(RL.DEFAULT_WAIT_LABEL)).toHaveValue(4);
    expect(screen.getByLabelText(RL.PAUSE_IDLE_LABEL)).toHaveValue(30);
    expect(unitPicker(RL.MAX_END_LABEL)).toHaveTextContent("days");
    expect(unitPicker(RL.DEFAULT_END_LABEL)).toHaveTextContent("days");
    expect(unitPicker(RL.MAX_WAIT_LABEL)).toHaveTextContent("hours");
    expect(unitPicker(RL.DEFAULT_WAIT_LABEL)).toHaveTextContent("hours");
    expect(unitPicker(RL.PAUSE_IDLE_LABEL)).toHaveTextContent("minutes");
    expect(screen.getByRole("switch", { name: RL.ALLOW_NO_END_LABEL })).toBeChecked();
    expect(screen.getByRole("switch", { name: RL.USER_CHANGES_LABEL })).toBeChecked();
  });

  // The API takes any number of seconds, so a stored value need not be a
  // whole number of the row's unit. Rounding it to that unit showed 1800s as
  // "1 hours" and 1200s as blank ("deployment ceiling") — a limit other than
  // the one stored, which a retype would then save.
  it("a stored value that is not a whole number of the row's unit shows in the largest unit it is", () => {
    renderEditor({
      ...GREENFIELD,
      limits: { max_wait_sec: 1800, default_wait_sec: 1200, pause_idle_after_sec: 20 },
    });

    expect(screen.getByLabelText(RL.MAX_WAIT_LABEL)).toHaveValue(30);
    expect(unitPicker(RL.MAX_WAIT_LABEL)).toHaveTextContent("minutes");
    expect(screen.getByLabelText(RL.DEFAULT_WAIT_LABEL)).toHaveValue(20);
    expect(unitPicker(RL.DEFAULT_WAIT_LABEL)).toHaveTextContent("minutes");
    expect(screen.getByLabelText(RL.PAUSE_IDLE_LABEL)).toHaveValue(20);
    expect(unitPicker(RL.PAUSE_IDLE_LABEL)).toHaveTextContent("seconds");
  });

  it("a sub-hour wait is entered with the unit picker, and the number typed is kept", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderEditor({ ...GREENFIELD, limits: {} });

    await userEvent.type(screen.getByLabelText(RL.MAX_WAIT_LABEL), "30");
    await userEvent.click(unitPicker(RL.MAX_WAIT_LABEL));
    // Seconds are offered only when a stored value needs them.
    expect(screen.queryByRole("option", { name: "seconds" })).toBeNull();
    await userEvent.click(await screen.findByRole("option", { name: "minutes" }));
    expect(screen.getByLabelText(RL.MAX_WAIT_LABEL)).toHaveValue(30);
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(updateProfileMock).toHaveBeenCalledWith(
      GREENFIELD.id,
      expect.objectContaining({ limits: { max_wait_sec: 30 * 60 } }),
    );
  });

  it("editing a duration row converts back to seconds on save, alongside the switches", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderEditor({ ...GREENFIELD, limits: {} });

    const maxEnd = screen.getByLabelText(RL.MAX_END_LABEL);
    await userEvent.clear(maxEnd);
    await userEvent.type(maxEnd, "14");
    await userEvent.click(screen.getByRole("switch", { name: RL.USER_CHANGES_LABEL }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(updateProfileMock).toHaveBeenCalledWith(
      GREENFIELD.id,
      expect.objectContaining({ limits: { max_end_ahead_sec: 14 * 86400, user_changes_limits: true } }),
    );
  });
});

// #93/#96 — the rubric round-trips through the editor: a saved row shows its
// level, and editing a DIFFERENT row on save carries both, so the write is a
// merge onto the loaded rubric rather than a fresh object with everything but
// the one field just touched dropped.
describe("ProfileEditor — the autonomy rubric round-trips", () => {
  beforeEach(() => {
    getSetupStatusMock.mockReset();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    gradePolicyMock.mockReset();
    gradePolicyMock.mockResolvedValue({ overall_risk: "medium", risk_assessment: [] });
  });

  it("shows a saved row's level and writes an edited row's on save, alongside it", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderEditor({ ...GREENFIELD, limits: { autonomy_rubric: { secrets_powerful: "L1" } } });

    expect(screen.getByText(RUBRIC.HEADING)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: `${RUBRIC.ROWS.secrets_powerful[0]} caps autonomy at` })).toHaveTextContent(
      AUTONOMY_META.L1.label,
    );
    // An untouched row reads "No cap" — the wire's absent field, not L0.
    expect(screen.getByRole("combobox", { name: `${RUBRIC.ROWS.confinement_cc1[0]} caps autonomy at` })).toHaveTextContent(RUBRIC.NOCAP);

    await userEvent.click(screen.getByRole("combobox", { name: `${RUBRIC.ROWS.confinement_cc1[0]} caps autonomy at` }));
    await userEvent.click(await screen.findByRole("option", { name: AUTONOMY_META.L2.label }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(updateProfileMock).toHaveBeenCalledWith(
      GREENFIELD.id,
      expect.objectContaining({
        limits: expect.objectContaining({
          autonomy_rubric: expect.objectContaining({ secrets_powerful: "L1", confinement_cc1: "L2" }),
        }),
      }),
    );
  });

  it("a profile with no rubric at all leaves every row at No cap and saves no autonomy_rubric untouched", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderEditor({ ...GREENFIELD, limits: {} });

    expect(screen.getByRole("combobox", { name: `${RUBRIC.ROWS.egress_open[0]} caps autonomy at` })).toHaveTextContent(RUBRIC.NOCAP);
    expect(screen.getByText(RUBRIC.EMPTY_NOTE)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    const call = updateProfileMock.mock.calls[0][1];
    expect(call.limits.autonomy_rubric).toBeUndefined();
  });
});
