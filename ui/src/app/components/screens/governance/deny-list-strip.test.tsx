/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The name strips under the deny-list boxes, and the standalone profile's two rows. The new strings are written
// out, not read from the copy module, so a drift in copy/deny-lists.ts fails this suite.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

const listComponentsMock = vi.fn();
vi.mock("../../../lib/api/components", () => ({ components: { list: (...a: unknown[]) => listComponentsMock(...a) } }));
const getProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: (...a: unknown[]) => getProvidersMock(...a) },
}));

import type { GovernanceUnit } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type { RunPolicySpec } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { DenyListRow, DenyListStrip } from "./deny-list-strip";
import { ProfileEditor } from "./profile-editor";

const SPEC: RunPolicySpec = { allowed_domains: ["*.corp"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" };
const PROD = "3f6c9a1e-5b7d-4c21-9e0a-7d2b8c4f1a60";
const STAGING = "7b0e2d54-9c13-4a8f-b6e1-4d5a3f9c2e07";
const PUBLIC_API = "5d1e7b90-2c4a-4f68-8b13-a9c0e6d27f41";
const GATEWAY = "e08a3c71-6b52-4d19-8f0a-1c9d7e3b5a24";

const COMPONENTS = [
  { id: PROD, name: "prod-deploy" },
  { id: STAGING, name: "staging-deploy" },
];
const PROVIDERS = [
  { id: "pub", uid: PUBLIC_API, name: "Public API", kind: "custom_endpoint" },
  { id: "gw", uid: GATEWAY, name: "Corp gateway", kind: "custom_endpoint" },
  { id: "no-uid", name: "Old provider", kind: "custom_endpoint" },
];

const COMPONENTS_LABEL = "Denied components";
const PROVIDERS_LABEL = "Denied model providers";
const COMPONENTS_HINT =
  "Component ids people on this profile may not attach to a run. A run that attaches one is refused, and a queued run that does ends when it starts. Applies to stored components only; empty denies none.";
const PROVIDERS_HINT =
  "Model provider uids people on this profile may not use. A run that would use one is refused, and a queued run that would ends when it starts. Empty denies none.";

const row = (over: Partial<GovernanceUnit>): GovernanceUnit => ({
  id: "ps",
  name: "Solo",
  ceiling: SPEC,
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});

function renderStrip(props: Partial<React.ComponentProps<typeof DenyListStrip>> & { operator?: boolean } = {}) {
  const { operator = true, ...rest } = props;
  const onAdd = vi.fn();
  render(
    <OperatorProvider operator={operator} operatorResolved principal="alice">
      <DenyListStrip list="components" listed={[]} disabled={false} onAdd={onAdd} {...rest} />
    </OperatorProvider>,
  );
  return { onAdd };
}

function renderEditor(profile: GovernanceUnit | null) {
  render(
    <OperatorProvider operator operatorResolved principal="alice">
      <ProfileEditor profile={profile} profiles={[]} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />
    </OperatorProvider>,
  );
}

const save = () => screen.getByRole("button", { name: GOV.SAVE });

beforeEach(() => {
  for (const m of [createProfileMock, updateProfileMock, getDefaultPolicyMock, getSetupStatusMock, listComponentsMock, getProvidersMock]) m.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getDefaultPolicyMock.mockResolvedValue({ ...SPEC, allowed_domains: ["*"] });
  listComponentsMock.mockResolvedValue(COMPONENTS);
  getProvidersMock.mockResolvedValue({ providers: { providers: PROVIDERS }, connected: {}, etag: null });
  updateProfileMock.mockResolvedValue({ profile: row({}), warnings: [] });
});
afterEach(cleanup);

describe("the deny-list name strip", () => {
  it("lists the stored components by name with the first eight characters of the id, and adds the id", async () => {
    const { onAdd } = renderStrip();
    expect(await screen.findByText("Stored components")).toBeInTheDocument();
    const prod = screen.getByRole("button", { name: "Add prod-deploy" });
    expect(prod).toHaveTextContent("prod-deploy");
    expect(prod).toHaveTextContent("3f6c9a1e");
    expect(prod).toBeEnabled();
    expect(screen.getByRole("button", { name: "Add staging-deploy" })).toBeInTheDocument();
    await userEvent.click(prod);
    expect(onAdd).toHaveBeenCalledWith(PROD);
  });

  it("lists the providers that have a uid, adds the uid, and skips a provider without one", async () => {
    const { onAdd } = renderStrip({ list: "providers" });
    expect(await screen.findByText("Your model providers")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add Public API" })).toHaveTextContent("5d1e7b90");
    expect(screen.queryByRole("button", { name: "Add Old provider" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Add Corp gateway" }));
    expect(onAdd).toHaveBeenCalledWith(GATEWAY);
  });

  it("shows a listed id as taken: a disabled button, and an id no entry matches is left alone", async () => {
    const { onAdd } = renderStrip({ listed: [PROD, "9999aaaa-0000-4000-8000-000000000000"] });
    const prod = await screen.findByRole("button", { name: "Add prod-deploy" });
    expect(prod).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add staging-deploy" })).toBeEnabled();
    await userEvent.click(prod);
    expect(onAdd).not.toHaveBeenCalled();
    expect(screen.getAllByRole("button")).toHaveLength(2);
  });

  it("is disabled with the box", async () => {
    renderStrip({ disabled: true });
    expect(await screen.findByRole("button", { name: "Add prod-deploy" })).toBeDisabled();
  });

  it("draws nothing while the read is pending, then the strip when it answers", async () => {
    let answer: (v: typeof COMPONENTS) => void = () => {};
    listComponentsMock.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    renderStrip();
    expect(screen.queryByText("Stored components")).toBeNull();
    await act(async () => answer(COMPONENTS));
    expect(await screen.findByText("Stored components")).toBeInTheDocument();
  });

  it("draws nothing, and reads nothing, for a person who is not an operator", async () => {
    renderStrip({ operator: false });
    renderStrip({ operator: false, list: "providers" });
    await act(async () => {});
    expect(screen.queryByText("Stored components")).toBeNull();
    expect(screen.queryByText("Your model providers")).toBeNull();
    expect(listComponentsMock).not.toHaveBeenCalled();
    expect(getProvidersMock).not.toHaveBeenCalled();
  });

  it("draws nothing for a failed read and for an empty list", async () => {
    listComponentsMock.mockRejectedValue(new Error("403"));
    getProvidersMock.mockResolvedValue({ providers: { providers: [{ id: "no-uid", name: "Old provider", kind: "custom_endpoint" }] }, connected: {}, etag: null });
    renderStrip();
    renderStrip({ list: "providers" });
    await waitFor(() => expect(listComponentsMock).toHaveBeenCalled());
    await waitFor(() => expect(getProvidersMock).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByText("Stored components")).toBeNull();
    expect(screen.queryByText("Your model providers")).toBeNull();
  });
});

describe("the standalone deny-list row", () => {
  it("sends the trimmed lines, and the strip adds to the box", async () => {
    const onChange = vi.fn();
    render(
      <OperatorProvider operator operatorResolved principal="alice">
        <DenyListRow which="components" value={[]} disabled={false} onChange={onChange} />
      </OperatorProvider>,
    );
    const box = screen.getByLabelText(COMPONENTS_LABEL);
    expect(box).toHaveAttribute("placeholder", "One id per line");
    fireEvent.change(box, { target: { value: `  ${PROD}  \n\n ${STAGING}\n` } });
    expect(onChange).toHaveBeenLastCalledWith([PROD, STAGING]);
    fireEvent.change(box, { target: { value: " \n " } });
    expect(onChange).toHaveBeenLastCalledWith([]);
    await userEvent.click(await screen.findByRole("button", { name: "Add prod-deploy" }));
    expect(onChange).toHaveBeenLastCalledWith([PROD]);
    expect(box).toHaveValue(PROD);
  });
});

describe("the profile editor's standalone rows", { timeout: 20_000 }, () => {
  it("draws both deny rows with the built hints, after the allowed-models row", async () => {
    renderEditor(row({}));
    const components = await screen.findByLabelText(COMPONENTS_LABEL);
    const providers = screen.getByLabelText(PROVIDERS_LABEL);
    expect(screen.getByText(COMPONENTS_HINT)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS_HINT)).toBeInTheDocument();
    const models = screen.getByTestId("governance-limit-allowed-models");
    expect(models.compareDocumentPosition(components) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(components.compareDocumentPosition(providers) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(await screen.findByText("Stored components")).toBeInTheDocument();
    expect(await screen.findByText("Your model providers")).toBeInTheDocument();
  });

  it("saves the trimmed lines under limits, shows a stored list, and leaves the key out once emptied", async () => {
    renderEditor(row({ limits: { deny_components: [PROD] } }));
    expect(screen.getByLabelText(COMPONENTS_LABEL)).toHaveValue(PROD);
    fireEvent.change(screen.getByLabelText(PROVIDERS_LABEL), { target: { value: ` ${PUBLIC_API} \n` } });
    await userEvent.click(save());
    expect(updateProfileMock.mock.calls[0][1].limits).toEqual({ deny_components: [PROD], deny_model_providers: [PUBLIC_API] });

    fireEvent.change(screen.getByLabelText(COMPONENTS_LABEL), { target: { value: "" } });
    fireEvent.change(screen.getByLabelText(PROVIDERS_LABEL), { target: { value: "\n" } });
    await userEvent.click(save());
    expect(updateProfileMock.mock.calls[1][1].limits).toEqual({});
  });
});
