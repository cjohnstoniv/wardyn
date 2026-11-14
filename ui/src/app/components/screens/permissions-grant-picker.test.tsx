/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Add grant Value field for kinds Wardyn already lists: a picker by name
// whose value is the id the resolver compares, with "Every one (*)" first, and
// grants shown by that name. Kinds without a list keep free text.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../lib/api/core";

const getPermissionsMock = vi.fn();
const upsertGrantMock = vi.fn();
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getPermissions: () => getPermissionsMock(),
    upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
  },
}));
vi.mock("../../lib/api/people", () => ({ people: { list: () => Promise.resolve({ people: [] }) } }));
vi.mock("../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

const listWorkspacesMock = vi.fn();
vi.mock("../../lib/api/workspaces", () => ({ workspaces: { listWorkspaces: () => listWorkspacesMock() } }));
const listPoliciesMock = vi.fn();
vi.mock("../../lib/api/policies", () => ({ policies: { listPolicies: () => listPoliciesMock() } }));
const getModelProvidersMock = vi.fn();
vi.mock("../../lib/api/model-providers", () => ({ modelProviders: { getModelProviders: () => getModelProvidersMock() } }));
vi.mock("../../lib/api/components", () => ({ components: { list: () => Promise.reject(new HttpError(403, "no")) } }));
vi.mock("../../lib/api/providers", () => ({ providers: { getWorkspaceProviders: () => Promise.reject(new HttpError(403, "no")) } }));
vi.mock("../../lib/api/agent-providers", () => ({ agentProviders: { getAgentProviders: () => Promise.reject(new HttpError(403, "no")) } }));
vi.mock("../../lib/api/setup", () => ({ setup: { getSetupStatus: () => Promise.reject(new HttpError(403, "no")) } }));

import { KIND, PERM, PERM_EXTRA } from "../../lib/permissions-copy";
import { aheadByHours } from "../../lib/test-clock";
import { setField } from "../../../test/set-field";
import { PermissionsScreen } from "./permissions";

const WS_ID = "3f6c1a52-1111-4a7e-9a5e-0d7f3a9b2c10";
const WS_OTHER = "9b2e7d44-2222-4c1d-8e3f-5a6b7c8d9e01";

beforeEach(() => {
  getPermissionsMock.mockReset().mockResolvedValue({ grants: [], enforcement: {} });
  upsertGrantMock.mockReset().mockResolvedValue({ id: "x" });
  listWorkspacesMock.mockReset().mockResolvedValue([
    { id: WS_ID, name: "web-app" },
    { id: WS_OTHER, name: "data-jobs" },
  ]);
  listPoliciesMock.mockReset().mockResolvedValue([]);
  getModelProvidersMock.mockReset().mockResolvedValue({ providers: { providers: [{ id: "corp-gateway", name: "Corp gateway", kind: "custom_endpoint" }] } });
});

async function chooseKind(user: ReturnType<typeof userEvent.setup>, label: string) {
  await screen.findAllByText(PERM.ADD_TITLE);
  await user.click(screen.getByRole("combobox", { name: PERM.FIELD_CAPABILITY }));
  await user.click(await screen.findByRole("option", { name: label }));
}

function renderScreen() {
  return render(
    <MemoryRouter>
      <PermissionsScreen />
    </MemoryRouter>,
  );
}

describe("Add grant Value picker", () => {
  it("offers workspaces by name, Every one first, and writes the id", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.workspace.label);

    const picker = await screen.findByRole("combobox", { name: KIND.workspace.valueLabel });
    await user.click(picker);
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([PERM_EXTRA.PICKER_EVERY, "web-app", "data-jobs", PERM_EXTRA.PICKER_ENTER_ID]);
    await user.click(screen.getByRole("option", { name: "web-app" }));
    expect(screen.getByText(PERM_EXTRA.PICKER_HINT)).toBeInTheDocument();
  });

  it("writes the workspace's id for a subject of Everyone", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findAllByText(PERM.ADD_TITLE);
    await user.click(screen.getByRole("button", { name: PERM.SUBJECT_ALL }));
    await chooseKind(user, KIND.workspace.label);
    await user.click(await screen.findByRole("combobox", { name: KIND.workspace.valueLabel }));
    await user.click(await screen.findByRole("option", { name: "web-app" }));
    await user.click(screen.getByRole("button", { name: PERM.ADD_CTA }));
    await waitFor(() =>
      expect(upsertGrantMock).toHaveBeenCalledWith({ subject_type: "all", subject: "", capability: "workspace", value: WS_ID, effect: "allow" }),
    );
  });

  it("lets an id outside the fetched page be entered, and submits exactly that id", async () => {
    const outside = "7d1e5c90-3333-4b2a-9c4d-1e2f3a4b5c6d";
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findAllByText(PERM.ADD_TITLE);
    await user.click(screen.getByRole("button", { name: PERM.SUBJECT_ALL }));
    await chooseKind(user, KIND.workspace.label);
    await user.click(await screen.findByRole("combobox", { name: KIND.workspace.valueLabel }));
    const options = await screen.findAllByRole("option");
    expect(options[options.length - 1]).toHaveTextContent(PERM_EXTRA.PICKER_ENTER_ID);
    await user.click(options[options.length - 1]);

    const box = await screen.findByRole("textbox", { name: KIND.workspace.valueLabel });
    setField(box, outside);
    await user.click(screen.getByRole("button", { name: PERM.ADD_CTA }));
    await waitFor(() =>
      expect(upsertGrantMock).toHaveBeenCalledWith({ subject_type: "all", subject: "", capability: "workspace", value: outside, effect: "allow" }),
    );

    await user.click(screen.getByRole("button", { name: PERM_EXTRA.PICKER_BACK_TO_LIST }));
    expect(await screen.findByRole("combobox", { name: KIND.workspace.valueLabel })).toBeInTheDocument();
  });

  it("keeps text typed before the list arrives", async () => {
    const typed = "7d1e5c90-3333-4b2a-9c4d-1e2f3a4b5c6d";
    let release!: (v: unknown) => void;
    listWorkspacesMock.mockReturnValue(new Promise((r) => (release = r)));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.workspace.label);
    setField(await screen.findByRole("textbox", { name: KIND.workspace.valueLabel }), typed);
    release([{ id: WS_ID, name: "web-app" }]);
    // The back-to-list button only exists once the list has arrived; the typed text is still there.
    await screen.findByRole("button", { name: PERM_EXTRA.PICKER_BACK_TO_LIST });
    expect(screen.getByRole("textbox", { name: KIND.workspace.valueLabel })).toHaveValue(typed);
    expect(screen.queryByRole("combobox", { name: KIND.workspace.valueLabel })).toBeNull();
  });

  it("swaps to the picker when the list arrives and nothing was typed", async () => {
    let release!: (v: unknown) => void;
    listWorkspacesMock.mockReturnValue(new Promise((r) => (release = r)));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.workspace.label);
    expect(await screen.findByRole("textbox", { name: KIND.workspace.valueLabel })).toBeInTheDocument();
    release([{ id: WS_ID, name: "web-app" }]);
    expect(await screen.findByRole("combobox", { name: KIND.workspace.valueLabel })).toBeInTheDocument();
  });

  it("keeps free text for a kind with no list, and for a list that is empty", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.secret.label);
    expect(screen.getByRole("textbox", { name: KIND.secret.valueLabel })).toBeInTheDocument();

    await chooseKind(user, KIND.policy.label);
    expect(await screen.findByRole("textbox", { name: KIND.policy.valueLabel })).toBeInTheDocument();
    expect(screen.getByText(KIND.policy.valueHint)).toBeInTheDocument();
  });

  it("falls back to free text, with the provider hint, when a list can't be read", async () => {
    getModelProvidersMock.mockRejectedValue(new HttpError(403, "no"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.model_provider.label);
    expect(screen.getByRole("textbox", { name: KIND.model_provider.valueLabel })).toBeInTheDocument();
    expect(screen.getByText("The provider's id. Open the provider on Settings → Model providers to set who it is available to instead.")).toBeInTheDocument();
  });

  it("offers a model provider by its name, with its slug as the value", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await chooseKind(user, KIND.model_provider.label);
    await user.click(await screen.findByRole("combobox", { name: KIND.model_provider.valueLabel }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([PERM_EXTRA.PICKER_EVERY, "Corp gateway", PERM_EXTRA.PICKER_ENTER_ID]);
  });
});

describe("Existing grants show the item's name", () => {
  it("shows the name with the id on hover, and the raw value when unknown or a wildcard", async () => {
    const row = (id: string, value: string) => ({
      id,
      subject_type: "all",
      subject: "",
      capability: "workspace",
      value,
      effect: "allow",
      created_at: aheadByHours(-1),
    });
    getPermissionsMock.mockResolvedValue({
      grants: [row("a1", WS_ID), row("a2", "00000000-0000-4000-8000-000000000000"), row("a3", "*")],
      enforcement: {},
    });
    renderScreen();
    const name = await screen.findByText("web-app");
    expect(name).toHaveAttribute("title", WS_ID);
    const table = screen.getByRole("table");
    expect(within(table).getByText("00000000-0000-4000-8000-000000000000")).toBeInTheDocument();
    expect(within(table).getByText("*")).toBeInTheDocument();
  });
});

describe("Add grant while the grant is being written", () => {
  it("is read-only, so a value typed then is not cleared by its answer", async () => {
    upsertGrantMock.mockReturnValue(new Promise(() => {}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findAllByText(PERM.ADD_TITLE);
    setField(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), "alice@corp.example");
    setField(screen.getByLabelText(KIND.egress_host.valueLabel), "*.github.com");
    await user.click(screen.getByRole("button", { name: PERM.ADD_CTA }));
    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalled());
    expect(screen.getByLabelText(KIND.egress_host.valueLabel)).toBeDisabled();
    expect(screen.getByRole("textbox", { name: PERM.FIELD_WHO })).toBeDisabled();
    expect(screen.getByRole("button", { name: PERM.SUBJECT_GROUP })).toBeDisabled();
    expect(screen.getByRole("button", { name: PERM.EFFECT_DENY })).toBeDisabled();
    // The capability picker is not a native control: it must be disabled itself, not only by a fieldset.
    await user.click(screen.getByRole("combobox", { name: PERM.FIELD_CAPABILITY }));
    expect(screen.queryByRole("option")).toBeNull();
  });
});

describe("Add grant: a value a feature row fills in during a write", () => {
  it("stays when the earlier grant answers", async () => {
    let answer: ((v: unknown) => void) | undefined;
    upsertGrantMock.mockReturnValue(new Promise((done) => (answer = done)));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findAllByText(PERM.ADD_TITLE);
    setField(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), "alice@corp.example");
    setField(screen.getByLabelText(KIND.egress_host.valueLabel), "*.github.com");
    await user.click(screen.getByRole("button", { name: PERM.ADD_CTA }));
    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Add a grant" }));
    expect(screen.getByLabelText(KIND.feature.valueLabel)).toHaveValue("inbound");
    await act(async () => answer?.({ id: "x" }));
    expect(screen.getByLabelText(KIND.feature.valueLabel)).toHaveValue("inbound");
  });
});
