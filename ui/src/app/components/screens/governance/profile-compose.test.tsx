/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The profile editor's composition half (0.8.6, M3): the base picker, per-field
// overlay opt-in, the effective view and the server's refusals. Mounts the
// editor directly, like profile-editor.test.tsx.
import { fireEvent, render, screen, within } from "@testing-library/react";
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

const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a) },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

import { HttpError } from "../../../lib/api/core";
import type { GovernanceProfile } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type { RunPolicySpec } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { seedOverlayLimits } from "./profile-overlay";
import { ProfileEditor } from "./profile-editor";

const SPEC: RunPolicySpec = {
  allowed_domains: ["*.corp", "git.x"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
};

const row = (over: Partial<GovernanceProfile>): GovernanceProfile => ({
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
  effective: { ceiling: SPEC, limits: { deny_interactive: true } },
});

function renderEditor(profile: GovernanceProfile | null, profiles: GovernanceProfile[] = [BASE]) {
  const onSaved = vi.fn();
  render(<ProfileEditor profile={profile} profiles={profiles} disabled={false} onCancel={vi.fn()} onSaved={onSaved} onSubmitted={vi.fn()} />);
  return { onSaved };
}

// One change event per field, not one per keystroke: every keystroke
// re-renders the whole editor, which is what pushed these cases past the test
// timeout on a loaded CI runner.
const typeValue = (label: string, value: string) =>
  fireEvent.change(screen.getByLabelText(label), { target: { value } });

const pickBase = async (name: string) => {
  await userEvent.click(screen.getByRole("combobox", { name: GOV.FIELD_BASE }));
  await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${name}`) }));
};

beforeEach(() => {
  for (const m of [createProfileMock, updateProfileMock, getDefaultPolicyMock, getSetupStatusMock]) m.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getDefaultPolicyMock.mockResolvedValue({ ...SPEC, allowed_domains: ["*"], allowed_methods: ["GET", "POST"] });
  createProfileMock.mockResolvedValue({ profile: BASE, warnings: [] });
  updateProfileMock.mockResolvedValue({ profile: BASE, warnings: [] });
});

// Budget measured: these cases cost 0.4-0.9s of pure render CPU on an idle
// core and 5-8s with a busy neighbour on it (each click re-renders the whole
// editor). A timed-out case also leaks its pending clicks into the next one.
describe("ProfileEditor — authoring an overlay", { timeout: 20_000 }, () => {
  it("on a profile base: an untouched field is absent, a narrowed one is the edited list", async () => {
    renderEditor(null);
    typeValue(GOV.FIELD_NAME, "Team A");
    await pickBase("Baseline");

    // The overlay lead replaces the JSON editor, and the inherited value is the base's effective one.
    expect(screen.getByText(GOV.OVERLAY_LEAD)).toBeInTheDocument();
    expect(screen.getAllByText(GOV.OVERLAY_INHERITED("*.corp, git.x")).length).toBeGreaterThan(0);
    expect(screen.queryByLabelText("Spec (JSON)")).toBeNull();

    // Saved untouched: an overlay object with NO keys, and no overlay_limits.
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(createProfileMock).toHaveBeenLastCalledWith({
      name: "Team A",
      ceiling: {},
      limits: {},
      base_profile_id: "pb",
      overlay: {},
      overlay_limits: null,
    });

    await userEvent.click(screen.getByRole("switch", { name: `allowed_domains ${GOV.OVERLAY_NARROW}` }));
    typeValue("allowed_domains", "*.corp");
    await userEvent.click(screen.getByRole("switch", { name: `${GOV.LIMIT_EXEC_LABEL} ${GOV.OVERLAY_NARROW}` }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(createProfileMock).toHaveBeenLastCalledWith(
      expect.objectContaining({
        overlay: { allowed_domains: ["*.corp"] },
        overlay_limits: { deny_task_mode_exec: true },
      }),
    );
  });

  it("on the deployment base: the inherited value is the deployment's, and base_profile_id is null", async () => {
    renderEditor(null);
    typeValue(GOV.FIELD_NAME, "Division");
    await pickBase(GOV.BASE_DEPLOYMENT);

    expect((await screen.findAllByText(GOV.OVERLAY_INHERITED("*"))).length).toBeGreaterThan(0);
    await userEvent.click(screen.getByRole("switch", { name: `allowed_methods ${GOV.OVERLAY_NARROW}` }));
    // Seeded from what is inherited, never an empty list.
    expect(screen.getByLabelText("allowed_methods")).toHaveValue("GET\nPOST");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(createProfileMock).toHaveBeenLastCalledWith(
      expect.objectContaining({ base_profile_id: null, overlay: { allowed_methods: ["GET", "POST"] } }),
    );
  });

  it("clearing a field back to inherited removes its key, and an emptied limits overlay is sent as null", async () => {
    const child = row({
      id: "pc",
      name: "Team A",
      ceiling: {} as RunPolicySpec,
      base_profile_id: "pb",
      overlay: { allowed_domains: ["*.corp"], some_future_field: 1 } as never,
      overlay_limits: { deny_interactive: true },
    });
    renderEditor(child, [BASE, child]);

    expect(screen.getByRole("switch", { name: `allowed_domains ${GOV.OVERLAY_NARROW}` })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await userEvent.click(screen.getByRole("switch", { name: `allowed_domains ${GOV.OVERLAY_NARROW}` }));
    await userEvent.click(screen.getByRole("switch", { name: `${GOV.LIMIT_INTERACTIVE_LABEL} ${GOV.OVERLAY_NARROW}` }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    // The key the console has no row for survives; the two cleared ones are gone.
    expect(updateProfileMock).toHaveBeenCalledWith("pc", {
      name: "Team A",
      ceiling: {},
      limits: {},
      base_profile_id: "pb",
      overlay: { some_future_field: 1 },
      overlay_limits: null,
    });
  });

  it("composing a standalone profile moves its stored limits and rubric into the overlay, with a note", async () => {
    const solo = row({
      id: "ps",
      name: "Solo",
      limits: {
        deny_interactive: true,
        deny_ui_apps: false,
        max_cpu_millis: 2000,
        max_memory_mib: 0,
        max_wait_sec: 3600,
        autonomy_rubric: { egress_open: "L1" },
      } as never,
    });
    renderEditor(solo, [BASE, solo]);
    expect(screen.queryByTestId("governance-limits-moved")).toBeNull();
    await pickBase("Baseline");
    expect(screen.getByTestId("governance-limits-moved")).toHaveTextContent(GOV.OVERLAY_LIMITS_MOVED);
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith(
      "ps",
      expect.objectContaining({
        limits: {},
        overlay_limits: {
          deny_interactive: true,
          max_cpu_millis: 2000,
          max_wait_sec: 3600,
          allow_no_end: false,
          user_changes_limits: false,
          autonomy_rubric: { egress_open: "L1" },
        },
      }),
    );
  });

  it("a stored allow_no_end or user_changes_limits that is on is not seeded", () => {
    const seeded = seedOverlayLimits({ allow_no_end: true, user_changes_limits: true } as never);
    expect(seeded).not.toHaveProperty("allow_no_end");
    expect(seeded).not.toHaveProperty("user_changes_limits");
  });

  it("a composed profile can narrow deny_ui_apps, the CPU ceiling and the autonomy rubric", async () => {
    renderEditor(null);
    typeValue(GOV.FIELD_NAME, "Team A");
    await pickBase("Baseline");
    await userEvent.click(screen.getByRole("switch", { name: `deny_ui_apps ${GOV.OVERLAY_NARROW}` }));
    await userEvent.click(screen.getByRole("switch", { name: `max_cpu_millis ${GOV.OVERLAY_NARROW}` }));
    typeValue("max_cpu_millis", "500");
    await userEvent.click(screen.getAllByRole("combobox", { name: /caps autonomy at/ })[0]);
    await userEvent.click(await screen.findByRole("option", { name: "Gated" }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    const sent = createProfileMock.mock.calls[0][0];
    expect(sent.overlay_limits.deny_ui_apps).toBe(true);
    expect(sent.overlay_limits.max_cpu_millis).toBe(500);
    expect(Object.keys(sent.overlay_limits.autonomy_rubric)).toHaveLength(1);
  });

  it("choosing None on a composed profile clears the composition and sends a full ceiling", async () => {
    const child = row({ id: "pc", name: "Team A", ceiling: {} as RunPolicySpec, base_profile_id: "pb", overlay: {} });
    renderEditor(child, [BASE, child]);
    await pickBase("None");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(updateProfileMock).toHaveBeenCalledWith(
      "pc",
      expect.objectContaining({ base_profile_id: null, overlay: null, overlay_limits: null }),
    );
    expect(updateProfileMock.mock.calls[0][1].ceiling.min_confinement_class).toBeTruthy();
  });

  it("a standalone profile saves with none of the composition fields", async () => {
    renderEditor(row({ id: "ps", name: "Solo" }), [BASE]);
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));
    expect(Object.keys(updateProfileMock.mock.calls[0][1]).sort()).toEqual(["ceiling", "limits", "name"]);
  });
});

describe("ProfileEditor — the base picker", () => {
  it("disables a descendant and a base that would make the chain too deep, each with its hint", async () => {
    const self = row({ id: "s", name: "Self", ceiling: {} as RunPolicySpec, base_profile_id: "pb", overlay: {} });
    const kid = row({ id: "k", name: "Kid", base_profile_id: "s", overlay: {} });
    const mid = row({ id: "m", name: "Mid", base_profile_id: "pb", overlay: {} });
    const deep = row({ id: "d", name: "Deep", base_profile_id: "m", overlay: {} });
    renderEditor(self, [BASE, self, kid, mid, deep]);
    await userEvent.click(screen.getByRole("combobox", { name: GOV.FIELD_BASE }));

    const kidOpt = await screen.findByRole("option", { name: new RegExp("^Kid") });
    expect(kidOpt).toHaveAttribute("aria-disabled", "true");
    expect(within(kidOpt).getByText(GOV.BASE_DESCENDANT)).toBeInTheDocument();
    // Self has a child (height 2), Deep sits at depth 3: 3 + 2 > 3.
    const deepOpt = screen.getByRole("option", { name: new RegExp("^Deep") });
    expect(deepOpt).toHaveAttribute("aria-disabled", "true");
    expect(within(deepOpt).getByText(GOV.BASE_TOO_DEEP)).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Baseline" })).not.toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByRole("option", { name: /^Self/ })).toBeNull();
  });
});

describe("ProfileEditor — the effective view", () => {
  it("shows the API's effective ceiling and limits, read-only", () => {
    const child = row({
      id: "pc",
      name: "Team A",
      ceiling: {} as RunPolicySpec,
      base_profile_id: "pb",
      overlay: {},
      effective: {
        ceiling: { ...SPEC, allowed_domains: ["*.corp"] },
        limits: { deny_interactive: true, max_concurrent_runs: 2 },
      },
    });
    renderEditor(child, [BASE, child]);
    const view = screen.getByTestId("governance-profile-effective");
    expect(within(view).getByText(GOV.EFFECTIVE_TITLE)).toBeInTheDocument();
    expect(within(view).getByText(GOV.EFFECTIVE_LEAD)).toBeInTheDocument();
    expect(within(view).getByTestId("governance-effective-allowed_domains")).toHaveTextContent("*.corp");
    expect(within(view).getByText(GOV.LIMIT_INTERACTIVE_LABEL)).toBeInTheDocument();
    expect(within(view).getByText(GOV.LIMIT_QUOTA_LABEL(2))).toBeInTheDocument();
    expect(within(view).queryByRole("textbox")).toBeNull();
  });

  it("a new or standalone profile shows no effective view", () => {
    renderEditor(null);
    expect(screen.queryByTestId("governance-profile-effective")).toBeNull();
  });
});

describe("ProfileEditor — the server's refusals", { timeout: 20_000 }, () => {
  it.each([
    ["governance_overlay_invalid", 400, GOV.REFUSED_OVERLAY_INVALID],
    ["governance_profile_cycle", 409, GOV.REFUSED_CYCLE],
    ["governance_profile_depth", 409, GOV.REFUSED_DEPTH],
    ["governance_overlay_unsatisfiable", 409, GOV.REFUSED_UNSATISFIABLE],
  ])("%s heads the server's sentence, verbatim", async (reason, status, title) => {
    const sentence = `server says ${reason} here`;
    createProfileMock.mockRejectedValue(new HttpError(status, sentence, reason));
    renderEditor(null);
    typeValue(GOV.FIELD_NAME, "Team A");
    await pickBase("Baseline");
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(title)).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
  });
});
