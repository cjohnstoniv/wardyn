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
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { ProfileEditor } from "./profile-editor";

const GREENFIELD: GovernanceProfile = {
  id: "p1",
  name: "Greenfield contractors",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
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

  it("renders all three rows with label, hint and the 0-means-unlimited wording", async () => {
    renderEditor();

    expect(screen.getByText(GOV.LIMIT_CONCURRENT_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_CONCURRENT_HINT)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_EPHEMERAL_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_EPHEMERAL_HINT)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_DRIVE_SIZE_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_DRIVE_SIZE_HINT)).toBeInTheDocument();

    // Every hint states the same zero rule — one across all three, not three
    // different sentences a reader has to reconcile.
    for (const hint of [GOV.LIMIT_CONCURRENT_HINT, GOV.LIMIT_EPHEMERAL_HINT, GOV.LIMIT_DRIVE_SIZE_HINT]) {
      expect(hint).toMatch(/0 means no limit/);
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
