/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The profile editor registers with the shared unsaved guard while it holds edits.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import type { GovernanceProfile } from "../../../lib/api/governance";
import type { SetupModelProvider } from "../../../lib/types";
import { baseStatus } from "../../../lib/test-fixtures";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import { specToSource } from "../../wardyn/policy-document/policy-source";
import { minimalSpec } from "../../wardyn/policy-panel";
import { ProfileEditor } from "./profile-editor";

// SafetyMeter (inside PolicyPanel) debounces a POST /policies/grade — stubbed so no test touches the network.
vi.mock("../../../lib/api/runs", () => ({
  runs: { gradePolicy: vi.fn().mockResolvedValue({ overall_risk: "medium", risk_assessment: [] }) },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

const GREENFIELD: GovernanceProfile = {
  id: "p1",
  name: "Greenfield contractors",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};

function renderEditor(profile: GovernanceProfile | null) {
  return render(<ProfileEditor profile={profile} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);
}
const nameField = () => document.getElementById("governance-profile-name")!;

beforeEach(() => {
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
});
afterEach(() => cleanup());

describe("ProfileEditor unsaved guard", () => {
  it("an existing profile: clean, dirty once renamed, clean again when the name is typed back", async () => {
    renderEditor(GREENFIELD);
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(unsavedSnapshot()).toBeNull();
    setField(nameField(), "Greenfield contractors 2");
    expect(unsavedSnapshot()).toContain("Greenfield contractors 2");
    setField(nameField(), "Greenfield contractors");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("a new profile whose starter is re-seeded from the fetched providers is not an edit", async () => {
    const providers: SetupModelProvider[] = [{ id: "oai", kind: "openai_api_key", host: "api.openai.com", harnesses: ["codex-cli"] }];
    expect(specToSource(minimalSpec(providers))).not.toBe(specToSource(minimalSpec()));
    getSetupStatusMock.mockResolvedValue(baseStatus({ model_providers: providers }));
    renderEditor(null);
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 0));
    expect(unsavedSnapshot()).toBeNull();
    setField(nameField(), "Contractors");
    expect(unsavedSnapshot()).not.toBeNull();
  });

  it("unregisters on unmount", async () => {
    const { unmount } = renderEditor(GREENFIELD);
    setField(nameField(), "Renamed");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
