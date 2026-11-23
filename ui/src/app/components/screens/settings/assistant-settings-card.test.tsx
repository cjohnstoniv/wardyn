/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings card for the assistant block: what it shows by default, the exact body and If-Match it
// sends, the providers that cannot serve, and the two answers to a save: applied (200) and held (202).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { expandCard } from "../../../lib/test-dom";
import { expectActionBar } from "../../../../test/action-bar";
import { setField } from "../../../../test/set-field";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

const getSettingsMock = vi.fn();
const putSettingsMock = vi.fn();
vi.mock("../../../lib/api/assistant", () => ({
  assistant: { getAssistantSettings: () => getSettingsMock(), putAssistantSettings: (...a: unknown[]) => putSettingsMock(...a) },
}));
const getProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({ modelProviders: { getModelProviders: () => getProvidersMock() } }));

import { HttpError } from "../../../lib/api/core";
import type { AssistantSettings, ModelProvider } from "../../../lib/types/site";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { AssistantSettingsCard, settingsBody } from "./assistant-settings-card";

const CLAUDE = [{ harness: "claude-code", model: "m" }];
const provider = (id: string, extra: Partial<ModelProvider> = {}): ModelProvider => ({
  id,
  name: id,
  kind: "custom_endpoint",
  harnesses: CLAUDE,
  ...extra,
});
const PROVIDERS: ModelProvider[] = [
  provider("org-gw"),
  provider("local", { kind: "local_model" }),
  provider("no-claude", { harnesses: [{ harness: "codex-cli" }] }),
  provider("bedrock", { kind: "bedrock_sso" }),
  provider("gw-checks", { gateway_identity: { audience: "a", inspection: "gateway" } }),
];
const OFF: AssistantSettings = { compose: false, review: false, run_history_counts: false, never_sent_kinds: [], providers: [] };

function renderCard() {
  return render(
    <MemoryRouter>
      <AssistantSettingsCard />
    </MemoryRouter>,
  );
}

async function open(settings: AssistantSettings = OFF) {
  getSettingsMock.mockResolvedValue({ settings, etag: '"a1"' });
  renderCard();
  await screen.findByRole("button", { name: new RegExp(`^${T.TITLE}`) });
  await expandCard(T.TITLE);
  await screen.findByText(T.MAY_SERVE);
  return screen.getByTestId("assistant-settings-card");
}

// The edit that every save test makes: Compose on, and org-gw marked for it.
async function widen(card: HTMLElement) {
  const user = userEvent.setup();
  await user.click(within(card).getByRole("switch", { name: T.COMPOSE }));
  await user.click(within(card).getByRole("checkbox", { name: `org-gw: ${T.COMPOSE}` }));
  return user;
}

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  getSettingsMock.mockReset();
  putSettingsMock.mockReset();
  getProvidersMock.mockReset().mockResolvedValue({ providers: { providers: PROVIDERS }, connected: {}, etag: null });
});

describe("settingsBody", () => {
  it("drops a provider with no purpose ticked and sorts both lists", () => {
    const body = settingsBody({
      compose: true,
      review: false,
      runHistory: false,
      kinds: ["role_mapping", "approval_rule"],
      marks: {
        b: { compose: true, review: false, models: "x, y" },
        a: { compose: false, review: true, models: "" },
        c: { compose: false, review: false, models: "z" },
      },
    });
    expect(body.never_sent_kinds).toEqual(["approval_rule", "role_mapping"]);
    expect(body.providers.map((p) => p.provider_id)).toEqual(["a", "b"]);
    expect(body.providers[1].models).toEqual(["x", "y"]);
  });
});

describe("AssistantSettingsCard", () => {
  it("by default every purpose is off and nothing can be saved", async () => {
    const card = await open();
    expect(screen.getByRole("button", { name: new RegExp(`^${T.TITLE}`) })).toHaveTextContent("Off");
    expect(within(card).getByText(T.PURPOSES_OFF)).toBeInTheDocument();
    expect(within(card).getByRole("switch", { name: T.COMPOSE })).not.toBeChecked();
    expect(within(card).getByRole("switch", { name: T.REVIEW })).not.toBeChecked();
    expect(within(card).getByRole("switch", { name: T.RUN_HISTORY })).not.toBeChecked();
    expect(within(card).getByText(T.ALWAYS_ON)).toBeInTheDocument();
    expect(within(card).getByText(T.BUILD_FILES)).toBeInTheDocument();
    const save = within(card).getByRole("button", { name: T.SAVE });
    expect(save).toBeDisabled();
    expectActionBar(save);
  });

  it("reads the stored block, and the summary names the purposes that are on", async () => {
    const card = await open({
      compose: true,
      review: false,
      run_history_counts: true,
      never_sent_kinds: ["role_mapping"],
      providers: [{ provider_id: "org-gw", compose: true, review: false, models: ["m-1"] }],
    });
    expect(screen.getByRole("button", { name: new RegExp(`^${T.TITLE}`) })).toHaveTextContent(T.COMPOSE);
    expect(within(card).getByRole("switch", { name: T.COMPOSE })).toBeChecked();
    expect(within(card).getByRole("switch", { name: T.RUN_HISTORY })).toBeChecked();
    expect(within(card).getByRole("checkbox", { name: `org-gw: ${T.COMPOSE}` })).toBeChecked();
    expect(within(card).getByRole("checkbox", { name: `org-gw: ${T.REVIEW}` })).not.toBeChecked();
    expect(within(card).getByRole("textbox", { name: `org-gw: ${T.COL_MODELS}` })).toHaveValue("m-1");
    expect(within(card).getByText("Role mapping")).toBeInTheDocument();
  });

  it("sends the whole block as JSON with the GET's ETag as If-Match", async () => {
    putSettingsMock.mockResolvedValue({ applied: { settings: OFF, etag: '"a2"' } });
    const card = await open();
    const user = await widen(card);
    setField(within(card).getByRole("textbox", { name: `org-gw: ${T.COL_MODELS}` }), "m-1, m-2");
    await user.selectOptions(within(card).getByRole("combobox", { name: T.ADD_KIND }), "role_mapping");
    await user.click(within(card).getByRole("button", { name: T.SAVE }));
    await waitFor(() => expect(putSettingsMock).toHaveBeenCalledTimes(1));
    expect(putSettingsMock).toHaveBeenCalledWith(
      {
        compose: true,
        review: false,
        run_history_counts: false,
        never_sent_kinds: ["role_mapping"],
        providers: [{ provider_id: "org-gw", compose: true, review: false, models: ["m-1", "m-2"] }],
      },
      '"a1"',
    );
  });

  it("a provider that cannot serve, a Bedrock provider and one that defers inspection show a reason and no checkboxes", async () => {
    const card = await open();
    const row = (id: string) => within(within(card).getByTestId(`assistant-provider-${id}`));
    expect(row("org-gw").getAllByRole("checkbox")).toHaveLength(2);
    expect(row("local").getAllByRole("checkbox")).toHaveLength(2);
    for (const [id, line] of [
      ["no-claude", T.CANNOT_SERVE],
      ["bedrock", "Bedrock providers can't serve the assistant, because Wardyn can't inspect their prompts."],
      ["gw-checks", T.DEFERS_INSPECTION],
    ] as const) {
      expect(row(id).getByText(line)).toBeInTheDocument();
      expect(row(id).queryAllByRole("checkbox")).toHaveLength(0);
      expect(row(id).queryByRole("textbox")).toBeNull();
    }
  });

  it("a 200 shows no held line, a saved confirmation and the new stored values", async () => {
    const stored: AssistantSettings = { ...OFF, compose: true, providers: [{ provider_id: "org-gw", compose: true, review: false, models: [] }] };
    putSettingsMock.mockResolvedValue({ applied: { settings: stored, etag: '"a2"' } });
    const card = await open();
    const user = await widen(card);
    await user.click(within(card).getByRole("button", { name: T.SAVE }));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST));
    expect(within(card).queryByText(T.HELD, { exact: false })).toBeNull();
    expect(within(card).getByRole("switch", { name: T.COMPOSE })).toBeChecked();
    expect(within(card).getByRole("button", { name: T.SAVE })).toBeDisabled();
  });

  it("a 202 says the change waits, links to it, shows today's stored values and is not a saved confirmation", async () => {
    putSettingsMock.mockResolvedValue({ held: true, changeId: "7f1c2d3e" });
    const card = await open();
    const user = await widen(card);
    await user.click(within(card).getByRole("button", { name: T.SAVE }));
    const held = await within(card).findByTestId("assistant-settings-held");
    expect(held).toHaveTextContent(T.HELD);
    expect(within(held).getByRole("link", { name: T.HELD_OPEN })).toHaveAttribute("href", "/governance/changes/7f1c2d3e");
    expect(toastSuccess).not.toHaveBeenCalled();
    // What is stored is still off: the form is back to it.
    expect(within(card).getByRole("switch", { name: T.COMPOSE })).not.toBeChecked();
    expect(within(card).getByRole("checkbox", { name: `org-gw: ${T.COMPOSE}` })).not.toBeChecked();
    expect(screen.getByRole("button", { name: new RegExp(`^${T.TITLE}`) })).toHaveTextContent("Off");
  });

  it("a stale save reloads the block and says someone else changed it", async () => {
    putSettingsMock.mockRejectedValue(new HttpError(412, "stale"));
    const card = await open();
    const user = await widen(card);
    getSettingsMock.mockResolvedValue({ settings: { ...OFF, review: true }, etag: '"a9"' });
    await user.click(within(card).getByRole("button", { name: T.SAVE }));
    expect(await within(card).findByRole("status")).toHaveTextContent("Someone else changed these settings");
    await waitFor(() => expect(within(card).getByRole("switch", { name: T.REVIEW })).toBeChecked());
    expect(within(card).getByRole("switch", { name: T.COMPOSE })).not.toBeChecked();
  });

  it("a refusal shows the server's sentence", async () => {
    putSettingsMock.mockRejectedValue(new HttpError(400, T.CANNOT_SERVE, "assistant_provider_cannot_serve"));
    const card = await open();
    const user = await widen(card);
    await user.click(within(card).getByRole("button", { name: T.SAVE }));
    expect(await within(card).findByRole("alert")).toHaveTextContent(T.CANNOT_SERVE);
  });

  it("a kind never sent can be removed again, and the choices leave out the kinds already listed", async () => {
    const card = await open({ ...OFF, never_sent_kinds: ["role_mapping"] });
    const user = userEvent.setup();
    const add = within(card).getByRole("combobox", { name: T.ADD_KIND });
    expect(within(add).queryByRole("option", { name: "Role mapping" })).toBeNull();
    await user.click(within(card).getByRole("button", { name: T.REMOVE_KIND("Role mapping") }));
    expect(within(card).queryByText("Role mapping", { selector: "span,div" })).toBeNull();
    expect(within(within(card).getByRole("combobox", { name: T.ADD_KIND })).getByRole("option", { name: "Role mapping" })).toBeInTheDocument();
  });
});
