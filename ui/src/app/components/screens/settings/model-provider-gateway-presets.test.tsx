/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The preset row on a new Your own endpoint provider, through the editor: which kinds show it, what a
// press fills, and what a save sends (never which preset was used).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS, PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { ModelProviderEditor, type ModelProviderEditorProps } from "./model-provider-editor";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../../../lib/api/health", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/health")>("../../../lib/api/health");
  return { ...actual, health: { ...actual.health, whoami: vi.fn() } };
});
const putMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];
const CHIPS = ["LiteLLM", "Portkey self-hosted", "Portkey hosted", "OpenRouter", "Other"];
const CORP: ModelProvider = {
  id: "corp-gateway",
  uid: "u-1",
  name: "Corp gateway",
  kind: "custom_endpoint",
  base_url: "https://gateway.corp.example",
  auth: { header: "Authorization", format: "Bearer %s" },
  extra_headers: [{ name: "X-Portkey-Provider", value: "anthropic" }],
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
};
const ANTHROPIC: ModelProvider = { id: "anthropic-api-key", uid: "u-2", name: "Anthropic API key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] };

const list = (providers: ModelProvider[] = []): ModelProvidersList => ({ providers: { providers }, etag: '"e1"', connected: {} });

function renderEditor(props: Partial<ModelProviderEditorProps> = {}) {
  const user = userEvent.setup();
  render(
    <OperatorProvider operator principal="alice">
      <ModelProviderEditor list={list()} editing={null} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} {...props} />
    </OperatorProvider>,
  );
  return { user };
}
async function startEndpoint() {
  const { user } = renderEditor();
  await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.custom_endpoint }));
  return user;
}
const group = () => screen.getByRole("radiogroup", { name: E.PRESET_LABEL });
const chip = (name: string) => within(group()).getByRole("radio", { name });
const press = (user: ReturnType<typeof userEvent.setup>, name: string) => user.click(chip(name));
const paths = () => screen.getAllByLabelText(E.PATH) as HTMLInputElement[];
const headerNames = () => screen.queryAllByPlaceholderText(E.HEADER_NAME_PLACEHOLDER) as HTMLInputElement[];

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});

describe("the preset row: where it shows", () => {
  it("on create, for Your own endpoint: the label, the hint and five chips in order, none pressed", async () => {
    await startEndpoint();
    expect(within(group()).getAllByRole("radio").map((b) => b.textContent)).toEqual(CHIPS);
    expect(screen.getByText(E.PRESET_HINT)).toBeInTheDocument();
    within(group()).getAllByRole("radio").forEach((b) => expect(b).toHaveAttribute("aria-checked", "false"));
    expect(screen.queryByText(/presidio/i)).toBeNull();
  });

  it("not on edit", () => {
    renderEditor({ editing: CORP, list: list([CORP]) });
    expect(screen.queryByRole("radiogroup", { name: E.PRESET_LABEL })).toBeNull();
    expect(screen.queryByText(E.PRESET_LABEL)).toBeNull();
  });

  it("not for another kind, and no header rows there either", async () => {
    const { user } = renderEditor();
    await user.click(screen.getByRole("button", { name: MODEL_PROVIDERS.KIND.anthropic_api_key }));
    expect(screen.queryByRole("radiogroup", { name: E.PRESET_LABEL })).toBeNull();
    expect(screen.queryByText(E.HEADERS_LABEL)).toBeNull();
    expect(screen.queryByRole("button", { name: E.HEADERS_ADD })).toBeNull();
  });

  it("editing a saved provider shows its header names in canonical case, and editing another kind shows none", () => {
    const { unmount } = render(
      <OperatorProvider operator principal="alice">
        <ModelProviderEditor list={list([CORP])} editing={CORP} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
      </OperatorProvider>,
    );
    expect(headerNames().map((i) => i.value)).toEqual(["X-Portkey-Provider"]);
    unmount();
    renderEditor({ editing: ANTHROPIC, list: list([ANTHROPIC]) });
    expect(screen.queryByText(E.HEADERS_LABEL)).toBeNull();
  });
});

describe("the preset row: pressed state", () => {
  it("the pressed chip is secondary and the others outline; pressing another moves it", async () => {
    const user = await startEndpoint();
    await press(user, "LiteLLM");
    expect(chip("LiteLLM")).toHaveAttribute("aria-checked", "true");
    expect(chip("LiteLLM")).toHaveClass("bg-secondary");
    for (const other of CHIPS.slice(1)) {
      expect(chip(other)).toHaveAttribute("aria-checked", "false");
      expect(chip(other)).not.toHaveClass("bg-secondary");
      expect(chip(other)).toHaveClass("border");
    }
    await press(user, "OpenRouter");
    expect(chip("LiteLLM")).toHaveAttribute("aria-checked", "false");
    expect(chip("OpenRouter")).toHaveClass("bg-secondary");
  });
});

describe("a press fills", () => {
  it("LiteLLM fills the two paths; they stay editable and stay edited", async () => {
    const user = await startEndpoint();
    await press(user, "LiteLLM");
    expect(paths().map((p) => p.value)).toEqual(["/", "/v1"]);
    setField(paths()[0], "/anthropic");
    expect(paths()[0]).toHaveValue("/anthropic");
    expect(chip("LiteLLM")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByLabelText(E.NAME)).toHaveValue(MODEL_PROVIDERS.KIND.custom_endpoint);
  });

  it("Portkey self-hosted ticks Claude Code, unticks Codex CLI and adds the x-portkey-provider row", async () => {
    const user = await startEndpoint();
    await press(user, "Portkey self-hosted");
    expect(screen.getByRole("checkbox", { name: "Claude Code" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Codex CLI" })).not.toBeChecked();
    expect(paths().map((p) => p.value)).toEqual(["/"]);
    expect(headerNames().map((i) => i.value)).toEqual(["x-portkey-provider"]);
    expect(screen.getAllByPlaceholderText(E.HEADER_VALUE_PLACEHOLDER).map((i) => (i as HTMLInputElement).value)).toEqual(["anthropic"]);
    await press(user, "Portkey self-hosted");
    expect(headerNames()).toHaveLength(1);
  });

  it("Portkey hosted sets the auth header and format, adds the empty row, and shows the slug hint only while pressed", async () => {
    const user = await startEndpoint();
    await press(user, "Portkey hosted");
    expect(screen.getByLabelText(E.AUTH_HEADER)).toHaveValue("x-portkey-api-key");
    expect(screen.getByLabelText(E.VALUE_FORMAT)).toHaveValue("{token}");
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("");
    expect(paths().map((p) => p.value)).toEqual(["", ""]);
    const slug = screen.getByPlaceholderText(E.PORTKEY_SLUG_PLACEHOLDER);
    expect(slug).toHaveValue("");
    expect(screen.getByText(E.PORTKEY_SLUG_HINT)).toBeInTheDocument();
    await press(user, "Other");
    expect(screen.queryByText(E.PORTKEY_SLUG_HINT)).toBeNull();
    expect(headerNames()).toHaveLength(1);
  });

  it("OpenRouter fills the base URL and paths, and gives the Codex CLI Model its placeholder and hint, only while pressed", async () => {
    const user = await startEndpoint();
    expect(screen.getAllByText(E.MODEL_HINT)).toHaveLength(2);
    await press(user, "OpenRouter");
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("https://openrouter.ai/api");
    expect(paths().map((p) => p.value)).toEqual(["/", "/v1"]);
    const models = screen.getAllByLabelText(E.MODEL) as HTMLInputElement[];
    expect(models.map((m) => m.placeholder)).toEqual(["", "openai/…"]);
    expect(screen.getByText(E.OPENROUTER_MODEL_HINT)).toBeInTheDocument();
    expect(screen.getAllByText(E.MODEL_HINT)).toHaveLength(1);
    await press(user, "LiteLLM");
    expect((screen.getAllByLabelText(E.MODEL) as HTMLInputElement[]).map((m) => m.placeholder)).toEqual(["", ""]);
    expect(screen.queryByText(E.OPENROUTER_MODEL_HINT)).toBeNull();
    expect(screen.getAllByText(E.MODEL_HINT)).toHaveLength(2);
  });

  it("Other changes no field", async () => {
    const user = await startEndpoint();
    await press(user, "Other");
    expect(chip("Other")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("");
    expect(screen.getByLabelText(E.AUTH_HEADER)).toHaveValue("Authorization");
    expect(paths().map((p) => p.value)).toEqual(["", ""]);
    expect(headerNames()).toHaveLength(0);
  });
});

describe("a save", () => {
  it("sends the header rows as name and value only, and nothing says which preset was pressed", async () => {
    const user = await startEndpoint();
    await press(user, "Portkey hosted");
    setField(screen.getByLabelText(E.BASE_URL), "https://gateway.corp.example");
    setField(paths()[0], "/");
    setField(paths()[1], "/v1");
    setField(screen.getByPlaceholderText(E.PORTKEY_SLUG_PLACEHOLDER), "@team");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const sent = (putMock.mock.calls[0][0] as { providers: ModelProvider[] }).providers[0];
    expect(sent.extra_headers).toStrictEqual([{ name: "x-portkey-provider", value: "@team" }]);
    expect(sent.auth).toEqual({ header: "x-portkey-api-key", format: "%s" });
    const text = JSON.stringify(sent);
    expect(text).not.toMatch(/placeholder|preset|portkey_hosted|litellm/i);
  });
});
