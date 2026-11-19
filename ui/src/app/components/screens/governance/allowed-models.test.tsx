/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Allowed models limit in the profile editor: the standalone row, its helper strip, and the composed row.
// Mounts the editor directly, like profile-compose.test.tsx.
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

const getProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: (...a: unknown[]) => getProvidersMock(...a) },
}));

import { HttpError } from "../../../lib/api/core";
import type { GovernanceUnit } from "../../../lib/api/governance";
import { ALLOWED_MODELS as AM, GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type { RunPolicySpec } from "../../../lib/types";
import type { ModelProvider } from "../../../lib/types/site";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { providerModels } from "./allowed-models";
import { ProfileEditor } from "./profile-editor";

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

const LIMITED_BASE = row({ id: "pb", name: "Baseline", effective: { ceiling: SPEC, limits: { allowed_models: ["model-a", "small-model-a"] } } });
const OPEN_BASE = row({ id: "po", name: "Open base", effective: { ceiling: SPEC, limits: {} } });

function renderEditor(profile: GovernanceUnit | null, profiles: GovernanceUnit[] = [LIMITED_BASE, OPEN_BASE], operator = true) {
  render(
    <OperatorProvider operator={operator} operatorResolved principal="alice">
      <ProfileEditor profile={profile} profiles={profiles} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />
    </OperatorProvider>,
  );
}

const typeValue = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const box = () => screen.getByLabelText(AM.LABEL);
const save = () => screen.getByRole("button", { name: GOV.SAVE });
const pickBase = async (name: string) => {
  await userEvent.click(screen.getByRole("combobox", { name: GOV.FIELD_BASE }));
  await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${name}`) }));
};

const PROVIDERS: ModelProvider[] = [
  {
    id: "foundry",
    name: "Foundry A",
    kind: "custom_endpoint",
    harnesses: [{ harness: "claude-code", model: "model-a", fast_model: "small-model-a" }],
  },
  { id: "gw", name: "Gateway B", kind: "custom_endpoint", harnesses: [{ harness: "claude-code", model: "model-b" }] },
  { id: "sub", name: "Subscription C", kind: "anthropic_subscription" },
  { id: "laptop", name: "Laptop D", kind: "local_model", harnesses: [{ harness: "claude-code", model: "local-model-a" }] },
  { id: "off", name: "Off E", kind: "custom_endpoint", disabled: true, harnesses: [{ harness: "claude-code", model: "model-off" }] },
];

beforeEach(() => {
  for (const m of [createProfileMock, updateProfileMock, getDefaultPolicyMock, getSetupStatusMock, getProvidersMock]) m.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getDefaultPolicyMock.mockResolvedValue({ ...SPEC, allowed_domains: ["*"] });
  getProvidersMock.mockResolvedValue({ providers: { providers: PROVIDERS }, connected: {}, etag: null });
  createProfileMock.mockResolvedValue({ profile: LIMITED_BASE, warnings: [] });
  updateProfileMock.mockResolvedValue({ profile: LIMITED_BASE, warnings: [] });
});
afterEach(cleanup);

describe("Allowed models: the standalone row", { timeout: 20_000 }, () => {
  it("is the last Limits row, with its hints, and says nothing about the proxy while the box is empty", async () => {
    renderEditor(row({}));
    await screen.findByTestId("governance-allowed-models-strip");
    expect(box()).toHaveAttribute("placeholder", AM.PLACEHOLDER);
    expect(screen.getByText(AM.HINT, { exact: false })).toBeInTheDocument();
    expect(screen.getByText(AM.SMALL_HINT)).toBeInTheDocument();
    expect(screen.queryByTestId("governance-allowed-models-note")).toBeNull();
    const rows = screen.getByTestId("governance-limit-allowed-models");
    expect(screen.getByTestId("governance-limit-drive-size").compareDocumentPosition(rows) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("saves the trimmed lines, shows the info note while the box is not empty, and leaves the key out once blank", async () => {
    renderEditor(row({ id: "ps", name: "Solo" }));
    typeValue(AM.LABEL, "  model-a  \n\n small-model-a\n");
    expect(screen.getByTestId("governance-allowed-models-note")).toHaveTextContent(AM.READS_NOTE);
    await userEvent.click(save());
    expect(updateProfileMock.mock.calls[0][1].limits).toEqual({ allowed_models: ["model-a", "small-model-a"] });

    typeValue(AM.LABEL, " \n ");
    expect(screen.queryByTestId("governance-allowed-models-note")).toBeNull();
    await userEvent.click(save());
    expect(updateProfileMock.mock.calls[1][1].limits).toEqual({});
  });

  it("shows a stored list and sends it back unchanged", async () => {
    renderEditor(row({ id: "ps", name: "Solo", limits: { allowed_models: ["model-a", "model-b"] } }));
    expect(box()).toHaveValue("model-a\nmodel-b");
    typeValue(GOV.FIELD_NAME, "Solo 2");
    await userEvent.click(save());
    expect(updateProfileMock.mock.calls[0][1].limits).toEqual({ allowed_models: ["model-a", "model-b"] });
  });

  it("shows the server's refusal sentence raw", async () => {
    const sentence = 'limits.allowed_models: "model-a" is listed twice';
    updateProfileMock.mockRejectedValue(new HttpError(400, sentence, "governance_invalid"));
    renderEditor(row({ id: "ps", name: "Solo" }));
    typeValue(AM.LABEL, "model-a\nmodel-a");
    await userEvent.click(save());
    expect(await screen.findByRole("alert")).toHaveTextContent(sentence);
  });
});

describe("Allowed models: the helper strip", { timeout: 20_000 }, () => {
  it("lists each hosted provider's models, never a local or disabled one, and adds a name on click", async () => {
    renderEditor(row({}));
    const strip = await screen.findByTestId("governance-allowed-models-strip");
    expect(within(strip).getByText(AM.STRIP_TITLE)).toBeInTheDocument();
    expect(within(strip).getByText(AM.SMALL_TAG)).toBeInTheDocument();
    expect(within(strip).queryByRole("button", { name: AM.ADD("local-model-a") })).toBeNull();
    expect(within(strip).queryByRole("button", { name: AM.ADD("model-off") })).toBeNull();

    await userEvent.click(within(strip).getByRole("button", { name: AM.ADD("model-a") }));
    expect(box()).toHaveValue("model-a");
    // The small model is only added when the admin clicks it.
    await userEvent.click(within(strip).getByRole("button", { name: AM.ADD("small-model-a") }));
    expect(box()).toHaveValue("model-a\nsmall-model-a");
    expect(within(strip).getByRole("button", { name: AM.ADD("model-a") })).toBeDisabled();
    await userEvent.click(save());
    expect(createProfileMock).not.toHaveBeenCalled();
  });

  it("says when a provider sets no small model, and when Wardyn does not hold its models", async () => {
    renderEditor(row({}));
    const strip = await screen.findByTestId("governance-allowed-models-strip");
    expect(within(strip).queryByText(AM.NO_SMALL("Foundry A"))).toBeNull();
    expect(within(strip).getByText(AM.NO_SMALL("Gateway B"))).toBeInTheDocument();
    expect(within(strip).getByText(AM.NOT_HELD("Subscription C"))).toBeInTheDocument();
  });

  it("asks for a small model only for a provider's Claude Code harness, and never on Azure Foundry", () => {
    const advised = (harnesses: ModelProvider["harnesses"], kind = "custom_endpoint") =>
      providerModels([{ id: "p", name: "P", kind, harnesses } as ModelProvider])[0].noSmall;
    expect(advised([{ harness: "codex-cli", model: "model-c" }])).toBe(false);
    expect(advised([{ harness: "codex-cli", model: "model-c" }, { harness: "claude-code", model: "model-a" }])).toBe(true);
    expect(advised([{ harness: "codex-cli", model: "model-c" }, { harness: "claude-code", model: "model-a", fast_model: "small-model-a" }])).toBe(false);
    expect(advised([{ harness: "claude-code", model: "model-a" }])).toBe(true);
    expect(advised([{ harness: "claude-code", model: "model-a" }], "azure_foundry")).toBe(false);
  });

  it("is absent, with the hints kept, when the providers cannot be read", async () => {
    getProvidersMock.mockRejectedValue(new HttpError(403, "forbidden", "forbidden"));
    renderEditor(row({}));
    await waitFor(() => expect(getProvidersMock).toHaveBeenCalled());
    expect(screen.queryByTestId("governance-allowed-models-strip")).toBeNull();
    expect(screen.getByText(AM.SMALL_HINT)).toBeInTheDocument();
  });

  it("is not even asked for by someone who is not an operator", async () => {
    renderEditor(row({}), [], false);
    expect(await screen.findByLabelText(AM.LABEL)).toBeInTheDocument();
    expect(getProvidersMock).not.toHaveBeenCalled();
    expect(screen.queryByTestId("governance-allowed-models-strip")).toBeNull();
  });
});

describe("Allowed models: the composed row", { timeout: 20_000 }, () => {
  const composed = async (base: string) => {
    renderEditor(null);
    typeValue(GOV.FIELD_NAME, "Team A");
    await pickBase(base);
  };
  const narrow = () => screen.getByRole("switch", { name: `${AM.LABEL} ${GOV.OVERLAY_NARROW}` });

  it("names what it inherits, and edits down from the inherited list", async () => {
    await composed("Baseline");
    const rowEl = screen.getByTestId("governance-overlay-limit-allowed_models");
    expect(within(rowEl).getByText(GOV.OVERLAY_INHERITED("model-a, small-model-a"))).toBeInTheDocument();
    expect(within(rowEl).getByText(AM.NARROW_HINT)).toBeInTheDocument();
    expect(within(rowEl).queryByText(AM.HINT)).toBeNull();
    await userEvent.click(narrow());
    expect(box()).toHaveValue("model-a\nsmall-model-a");
    expect(screen.getByTestId("governance-allowed-models-note")).toBeInTheDocument();
    typeValue(AM.LABEL, "model-a");
    await userEvent.click(save());
    expect(createProfileMock).toHaveBeenLastCalledWith(expect.objectContaining({ overlay_limits: { allowed_models: ["model-a"] } }));
  });

  it("where the base limits no models, says to choose and leaves Narrow off for no limit", async () => {
    await composed("Open base");
    const rowEl = screen.getByTestId("governance-overlay-limit-allowed_models");
    expect(within(rowEl).getByText(AM.NARROW_HINT_OPEN, { exact: false })).toBeInTheDocument();
    expect(within(rowEl).queryByText(AM.NARROW_HINT)).toBeNull();
  });

  it("with Narrow on and an empty box says so, holds Save, and never sends an empty list", async () => {
    await composed("Open base");
    await userEvent.click(narrow());
    expect(box()).toHaveValue("");
    expect(box()).toHaveAttribute("placeholder", AM.PLACEHOLDER);
    expect(screen.getByRole("alert")).toHaveTextContent(AM.NARROW_EMPTY);
    expect(save()).toBeDisabled();
    expect(createProfileMock).not.toHaveBeenCalled();

    typeValue(AM.LABEL, "model-b");
    expect(screen.queryByText(AM.NARROW_EMPTY)).toBeNull();
    expect(save()).toBeEnabled();
    await userEvent.click(save());
    expect(createProfileMock).toHaveBeenLastCalledWith(expect.objectContaining({ overlay_limits: { allowed_models: ["model-b"] } }));
  });

  it("turning Narrow off again leaves the key out", async () => {
    await composed("Open base");
    await userEvent.click(narrow());
    await userEvent.click(narrow());
    expect(save()).toBeEnabled();
    await userEvent.click(save());
    expect(createProfileMock.mock.calls[0][0].overlay_limits).toBeNull();
  });

  it("offers the providers' models under the box while Narrow is on, and adds one on click", async () => {
    await composed("Open base");
    expect(screen.queryByTestId("governance-allowed-models-strip")).toBeNull();
    await userEvent.click(narrow());
    const strip = await screen.findByTestId("governance-allowed-models-strip");
    expect(screen.getAllByTestId("governance-allowed-models-strip")).toHaveLength(1);
    expect(within(strip).queryByRole("button", { name: AM.ADD("local-model-a") })).toBeNull();
    await userEvent.click(within(strip).getByRole("button", { name: AM.ADD("model-b") }));
    expect(box()).toHaveValue("model-b");
    expect(within(strip).getByRole("button", { name: AM.ADD("model-b") })).toBeDisabled();
    typeValue(AM.LABEL, "model-b\nmodel-a");
    await userEvent.click(within(strip).getByRole("button", { name: AM.ADD("small-model-a") }));
    expect(box()).toHaveValue("model-b\nmodel-a\nsmall-model-a");
    await userEvent.click(save());
    expect(createProfileMock).toHaveBeenLastCalledWith(
      expect.objectContaining({ overlay_limits: { allowed_models: ["model-b", "model-a", "small-model-a"] } }),
    );
  });

  it("shows a refused composition's sentence raw", async () => {
    const sentence = "allowed_models: no model is in both the base (model-a, small-model-a) and the overlay (model-c)";
    createProfileMock.mockRejectedValue(new HttpError(409, sentence, "governance_overlay_unsatisfiable"));
    await composed("Baseline");
    await userEvent.click(narrow());
    typeValue(AM.LABEL, "model-c");
    await userEvent.click(save());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(GOV.REFUSED_UNSATISFIABLE)).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
  });

  it("the Effective view shows the list as a chip and the server's warning as it came", () => {
    const warning = "allowed_models: the base excludes model-c";
    const child = row({
      id: "pc",
      name: "Team A",
      ceiling: {} as RunPolicySpec,
      parent_id: "pb",
      overlay: {} as never,
      overlay_limits: { allowed_models: ["model-a", "model-c"] } as never,
      effective: { ceiling: SPEC, limits: { allowed_models: ["model-a"] }, warnings: [warning] },
    });
    renderEditor(child, [LIMITED_BASE, child]);
    const view = screen.getByTestId("governance-profile-effective");
    expect(within(view).getByText("Allowed models: model-a")).toBeInTheDocument();
    expect(within(view).getByText(warning)).toHaveClass("text-warning");
  });
});
