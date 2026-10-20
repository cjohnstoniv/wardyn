/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9: the local model server in the provider editor (chips, the token switch, the caps,
// the Model pick, the server's refusals in their place) and in the provider list.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../lib/api/core";
import type { ModelProvidersList as Snapshot } from "../../../lib/api/model-providers";
import { LOCAL_MODEL_EDITOR as L, LOCAL_MODEL_LIST } from "../../../lib/local-model-copy";
import { MODEL_PROVIDERS as M, PROVIDER_EDITOR as E, PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { setField } from "../../../../test/set-field";
import { expandCard } from "../../../lib/test-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelProviderEditor } from "./model-provider-editor";
import { ModelProvidersList } from "./model-providers-list";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
const getMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a), getModelProviders: () => getMock() },
}));
vi.mock("../../../lib/api/agent-providers", () => ({
  agentProviders: { getAgentProviders: () => Promise.resolve({ providers: { agents: [] }, etag: '"r"' }) },
}));

const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];
const LAPTOP: ModelProvider = {
  id: "laptop-gpu",
  name: "Laptop GPU",
  kind: "local_model",
  base_url: "http://host.docker.internal:11434",
  local: { models: ["qwen2.5-coder:7b", "llama3.2:3b"], max_output: 4096 },
  harnesses: [
    { harness: "claude-code", model: "qwen2.5-coder:7b" },
    { harness: "codex-cli", model: "qwen2.5-coder:7b" },
  ],
};
const NODE: ModelProvider = {
  id: "gpu-node",
  name: "GPU node",
  kind: "local_model",
  base_url: "https://gpu-node.corp.example:8443",
  auth: { header: "Authorization", format: "Bearer %s" },
  local: { models: ["a", "b", "c"] },
  harnesses: [{ harness: "claude-code", model: "a" }],
};

const snapshot = (providers: ModelProvider[] = []): Snapshot => ({ providers: { providers }, etag: '"e1"', connected: {} });
const putBody = () => putMock.mock.calls[0][0] as { providers: ModelProvider[] };

function renderEditor(editing: ModelProvider | null = null) {
  const user = userEvent.setup();
  const onSaved = vi.fn();
  render(
    <OperatorProvider operator principal="alice">
      <ModelProviderEditor list={snapshot()} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={onSaved} />
    </OperatorProvider>,
  );
  return { user, onSaved };
}

async function openLocal() {
  const r = renderEditor();
  await r.user.click(screen.getByRole("button", { name: M.KIND.local_model }));
  return r;
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
  getMock.mockReset();
});

describe("the local model server form", () => {
  it("opens with the board's fields: no token, no models, empty caps showing the defaults", async () => {
    await openLocal();
    expect(screen.getByText(L.DESCRIPTION)).toBeInTheDocument();
    expect(screen.getByLabelText(E.NAME)).toHaveValue(M.KIND.local_model);
    expect(screen.getByLabelText(E.BASE_URL)).toHaveValue("");
    expect(screen.getByText(L.BASE_URL_HINT)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: L.TOKEN_SWITCH })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText(L.TOKEN_OFF_HINT)).toBeInTheDocument();
    expect(screen.queryByLabelText(E.AUTH_HEADER)).toBeNull();
    expect(screen.getByText(L.MODELS_NONE)).toBeInTheDocument();
    expect(screen.getByText(L.MODELS_COUNT(0))).toBeInTheDocument();
    expect(screen.getByText(L.MODELS_HINT)).toBeInTheDocument();
    expect(screen.getByLabelText(L.CONTEXT)).toHaveAttribute("placeholder", "32768");
    expect(screen.getByLabelText(L.REPLY)).toHaveAttribute("placeholder", "32768");
    expect(screen.getByLabelText(L.KEEP_ALIVE)).toHaveAttribute("placeholder", "1800");
    expect(screen.getByText(L.LIMITS_HINT)).toBeInTheDocument();
    expect(screen.getByText(L.MODEL_HINT_CLAUDE)).toBeInTheDocument();
    expect(screen.getByText(L.MODEL_HINT)).toBeInTheDocument();
    // Both agents can drive a local model, and there is no Path.
    expect(screen.getByRole("checkbox", { name: "Claude Code" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Codex CLI" })).toBeChecked();
    expect(screen.queryByLabelText(E.PATH)).toBeNull();
  });

  it("chips: Enter, a comma and a paste of several names add; a repeat is dropped; remove takes one out", async () => {
    const { user } = await openLocal();
    const input = screen.getByPlaceholderText(L.MODELS_PLACEHOLDER);
    setField(input, "qwen2.5-coder:7b");
    await user.keyboard("{Enter}");
    setField(input, "llama3.2:3b,");
    await user.click(input);
    await user.paste("m1 m2,qwen2.5-coder:7b");
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "qwen2.5-coder:7b",
      "llama3.2:3b",
      "m1",
      "m2",
    ]);
    expect(screen.getByText(L.MODELS_COUNT(4))).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: L.MODEL_REMOVE("m1") }));
    expect(screen.getByText(L.MODELS_COUNT(3))).toBeInTheDocument();
    // Enter adds and never submits the form.
    expect(putMock).not.toHaveBeenCalled();
  });

  it("the Model pick lists the chips, defaults to the first, and a save writes the whole record", async () => {
    const { user, onSaved } = await openLocal();
    setField(screen.getByLabelText(E.NAME), "Laptop GPU");
    setField(screen.getByLabelText(E.BASE_URL), "http://host.docker.internal:11434");
    const input = screen.getByPlaceholderText(L.MODELS_PLACEHOLDER);
    setField(input, "qwen2.5-coder:7b");
    await user.keyboard("{Enter}");
    setField(input, "llama3.2:3b");
    await user.keyboard("{Enter}");
    setField(screen.getByLabelText(L.REPLY), "4096");
    const codex = screen.getAllByLabelText(E.MODEL)[1];
    expect(within(codex).getAllByRole("option").map((o) => o.textContent)).toEqual(["qwen2.5-coder:7b", "llama3.2:3b"]);
    await user.selectOptions(codex, "llama3.2:3b");
    await user.click(screen.getByRole("button", { name: E.SAVE }));

    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(putBody().providers).toEqual([
      {
        id: "laptop-gpu",
        name: "Laptop GPU",
        kind: "local_model",
        base_url: "http://host.docker.internal:11434",
        local: { models: ["qwen2.5-coder:7b", "llama3.2:3b"], max_output: 4096 },
        harnesses: [
          { harness: "claude-code", model: "qwen2.5-coder:7b" },
          { harness: "codex-cli", model: "llama3.2:3b" },
        ],
      },
    ]);
    // No token: no auth on the wire.
    expect(putBody().providers[0].auth).toBeUndefined();
  });

  it("the token switch shows Auth header and Value format, and a save sends auth", async () => {
    const { user } = await openLocal();
    await user.click(screen.getByRole("switch", { name: L.TOKEN_SWITCH }));
    expect(screen.getByLabelText(E.AUTH_HEADER)).toHaveValue("Authorization");
    expect(screen.getByLabelText(E.VALUE_FORMAT)).toHaveValue("Bearer {token}");
    expect(screen.getByText(E.PROVIDES_TOKEN)).toBeInTheDocument();
    expect(screen.queryByText(L.TOKEN_OFF_HINT)).toBeNull();
    setField(screen.getByLabelText(E.BASE_URL), "https://gpu-node.corp.example:8443");
    setField(screen.getByPlaceholderText(L.MODELS_PLACEHOLDER), "a");
    await user.keyboard("{Enter}");
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putBody().providers[0].auth).toEqual({ header: "Authorization", format: "Bearer %s" });
  });

  it("editing reopens the stored record: token on, chips, caps", () => {
    renderEditor({ ...NODE, local: { models: ["a", "b"], max_context: 8192, keep_alive_seconds: 600 } });
    expect(screen.getByRole("switch", { name: L.TOKEN_SWITCH })).toHaveAttribute("aria-checked", "true");
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["a", "b"]);
    expect(screen.getByLabelText(L.CONTEXT)).toHaveValue(8192);
    expect(screen.getByLabelText(L.REPLY)).toHaveValue(null);
    expect(screen.getByLabelText(L.KEEP_ALIVE)).toHaveValue(600);
    expect(screen.getByRole("button", { name: E.REMOVE })).toBeInTheDocument();
  });

  async function refusedWith(message: string, fill?: (user: ReturnType<typeof userEvent.setup>) => Promise<void>) {
    putMock.mockRejectedValue(new HttpError(400, message));
    const { user } = await openLocal();
    setField(screen.getByLabelText(E.BASE_URL), "http://host.docker.internal:11434");
    setField(screen.getByPlaceholderText(L.MODELS_PLACEHOLDER), "qwen2.5-coder:7b");
    await user.keyboard("{Enter}");
    await fill?.(user);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText(PROVIDERS.SAVE_REFUSED_TITLE_ONE)).toBeInTheDocument();
    expect(within(note).getByText(message)).toBeInTheDocument();
    return user;
  }

  it("a refused address is shown in the card and outlines the Base URL", async () => {
    await refusedWith(
      `model_providers: "laptop-gpu": base_url "http://localhost:11434" names the proxy's own loopback, not the machine the model server runs on — use host.docker.internal on a laptop, or the node's address`,
    );
    expect(screen.getByLabelText(E.BASE_URL)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(L.REPLY)).not.toHaveAttribute("aria-invalid");
  });

  it("a refused model name marks that chip, and the list keeps what was typed", async () => {
    await refusedWith(
      `model_providers: "laptop-gpu": local.models: "qwen2.5-coder:7b" is not a model name a run may use — letters, digits and ._:@+[]- , no "/" (a registry or namespace) and no cloud tag`,
    );
    expect(screen.getByText("qwen2.5-coder:7b", { selector: "li" })).toHaveAttribute("data-bad", "true");
    expect(screen.getByLabelText(E.BASE_URL)).not.toHaveAttribute("aria-invalid");
  });

  it("an empty list's refusal is shown beside the empty chip row", async () => {
    await refusedWith(
      `model_providers: "laptop-gpu": a local_model provider needs local.models — the exact model names its runs may use`,
      async (user) => {
        await user.click(screen.getByRole("button", { name: L.MODEL_REMOVE("qwen2.5-coder:7b") }));
      },
    );
    expect(screen.getByText(L.MODELS_NONE)).toBeInTheDocument();
  });

  it("a refused limit outlines that limit", async () => {
    await refusedWith(`model_providers: "laptop-gpu": local.max_output must be between 0 (the default, 32768) and 1048576`, async () => {
      setField(screen.getByLabelText(L.REPLY), "5000000");
    });
    expect(screen.getByLabelText(L.REPLY)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(L.CONTEXT)).not.toHaveAttribute("aria-invalid");
  });

  it("a refused agent model outlines that agent's pick and no other", async () => {
    await refusedWith(
      `model_providers: "laptop-gpu": codex-cli: model must be one of local.models — it is the model this agent asks for`,
    );
    const [claude, codex] = screen.getAllByLabelText(E.MODEL);
    expect(codex).toHaveAttribute("aria-invalid", "true");
    expect(claude).not.toHaveAttribute("aria-invalid");
    expect(screen.getByLabelText(E.BASE_URL)).not.toHaveAttribute("aria-invalid");
  });
});

describe("the provider list", () => {
  const given = (providers: ModelProvider[], connected: Record<string, number> = {}) =>
    getMock.mockResolvedValue({ providers: { providers }, connected, etag: '"e"' });
  const renderList = () =>
    render(
      <MemoryRouter>
        <ModelProvidersList harnesses={[{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true }]} />
      </MemoryRouter>,
    );

  it("a server with no token: its kind, no token, its address and model count, and 'No token needed'", async () => {
    given([LAPTOP]);
    renderList();
    await expandCard(M.TITLE);
    const row = within(await screen.findByTestId("model-provider-laptop-gpu"));
    expect(row.getByText(M.KIND.local_model)).toBeInTheDocument();
    expect(row.getByText(LOCAL_MODEL_LIST.PROVIDES_NONE)).toBeInTheDocument();
    expect(row.getByText(LOCAL_MODEL_LIST.ADDRESS("host.docker.internal:11434", 2))).toBeInTheDocument();
    expect(row.getByText(LOCAL_MODEL_LIST.CHIP_NO_TOKEN)).toBeInTheDocument();
    expect(row.queryByText(/Connected by|No one has connected/)).toBeNull();
  });

  it("a server that wants a token keeps the canon line and the connected count", async () => {
    given([NODE], { "gpu-node": 4 });
    renderList();
    await expandCard(M.TITLE);
    const row = within(await screen.findByTestId("model-provider-gpu-node"));
    expect(row.getByText(M.PROVIDES.TOKEN)).toBeInTheDocument();
    expect(row.getByText(LOCAL_MODEL_LIST.ADDRESS("gpu-node.corp.example:8443", 3))).toBeInTheDocument();
    expect(row.getByText(M.CONNECTED(4))).toBeInTheDocument();
  });

  it("one model reads as '1 model', and the empty state names the new kind", async () => {
    expect(LOCAL_MODEL_LIST.ADDRESS("h:1", 1)).toBe("h:1 · 1 model");
    given([]);
    renderList();
    await expandCard(M.TITLE);
    expect(await screen.findByText(LOCAL_MODEL_LIST.EMPTY_EXTRA)).toBeInTheDocument();
  });
});
