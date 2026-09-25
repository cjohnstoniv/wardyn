/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings → Model providers (#536): every state packet MP-A draws (A1–A9),
// plus loading and a failed read. The count on a row is connected[id] from
// GET /model-providers, 0 included.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getModelProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: () => getModelProvidersMock() },
}));
const getAgentProvidersMock = vi.fn();
vi.mock("../../../lib/api/agent-providers", () => ({
  agentProviders: { getAgentProviders: () => getAgentProvidersMock() },
}));

import type { ModelProvider } from "../../../lib/api/model-providers";
import type { AgentProvider } from "../../../lib/api/agent-providers";
import type { SetupHarnessTool } from "../../../lib/types";
import { MODEL_LEDE, MODEL_PROVIDERS as M, PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { ModelProvidersList } from "./model-providers-list";

const CLAUDE: SetupHarnessTool = { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true };
const CODEX: SetupHarnessTool = { id: "codex-cli", display: "Codex CLI", has_gateway: true, has_login: false, enabled: true };
const NONE: SetupHarnessTool = {
  id: "none", display: "Your own tools", has_gateway: false, has_login: false, no_managed_auth: true, enabled: true,
};

const bedrock: ModelProvider = { id: "bedrock-prod", name: "Bedrock (prod)", kind: "bedrock_sso", harnesses: [{ harness: "claude-code" }] };
const key: ModelProvider = { id: "anthropic-key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] };
const gateway: ModelProvider = {
  id: "corp-gateway",
  name: "Corp gateway",
  kind: "custom_endpoint",
  harnesses: [{ harness: "claude-code" }, { harness: "codex-cli" }],
};
const sub: ModelProvider = { id: "claude-sub", name: "Claude subscription", kind: "anthropic_subscription", harnesses: [{ harness: "claude-code" }] };
const openai: ModelProvider = { id: "openai-key", kind: "openai_api_key", harnesses: [{ harness: "codex-cli" }] };

function given(providers: ModelProvider[], connected: Record<string, number> = {}, roster: AgentProvider[] = []) {
  getModelProvidersMock.mockResolvedValue({ providers: { providers }, connected, etag: '"e"' });
  getAgentProvidersMock.mockResolvedValue({ providers: { agents: roster }, etag: '"r"' });
}

const def = (id: string, provider: string): AgentProvider => ({ id, mechanism: "none", default_provider: provider });

function renderList(harnesses: SetupHarnessTool[] = [CLAUDE, CODEX, NONE]) {
  return render(
    <MemoryRouter>
      <ModelProvidersList harnesses={harnesses} />
    </MemoryRouter>,
  );
}

const row = (id: string) => within(screen.getByTestId(`model-provider-${id}`));

beforeEach(() => {
  getModelProvidersMock.mockReset();
  getAgentProvidersMock.mockReset();
});

describe("ModelProvidersList", () => {
  it("A1: no providers is the empty state, under the title, lede and Add button", async () => {
    given([]);
    renderList();
    expect(await screen.findByText(M.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(M.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: M.TITLE })).toBeInTheDocument();
    expect(screen.getByText(MODEL_LEDE)).toBeInTheDocument();
    // No A9 notice over an empty list, even with agents turned on.
    expect(screen.queryByText(M.HARNESS_UNSERVED("Codex CLI"))).toBeNull();
  });

  it("Add model provider opens the editor (#537) at its kind step", async () => {
    given([]);
    renderList();
    await userEvent.click(await screen.findByRole("button", { name: M.ADD_CTA }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: M.ADD_CTA })).toBeInTheDocument();
    expect(within(dialog).getByRole("group", { name: PROVIDER_EDITOR.KIND_TITLE })).toBeInTheDocument();
  });

  it.each([
    ["a Bedrock row", bedrock, "Bedrock (prod)"],
    ["a Claude subscription row", sub, "Claude subscription"],
  ])("%s does not open the editor (#538 builds those kinds)", async (_, provider, name) => {
    given([provider]);
    renderList();
    const r = await screen.findByTestId(`model-provider-${provider.id}`);
    expect(within(r).queryByRole("button")).toBeNull();
    await userEvent.click(within(r).getByText(name));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a row opens the editor on that provider", async () => {
    given([gateway]);
    renderList();
    await userEvent.click(await screen.findByRole("button", { name: /^Corp gateway/ }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Corp gateway" })).toBeInTheDocument();
    expect(within(dialog).getByLabelText(PROVIDER_EDITOR.NAME)).toHaveValue("Corp gateway");
  });

  it("A2: one provider shows name, kind, what each person provides, Used by and its count, with no default chip", async () => {
    given([bedrock], { "bedrock-prod": 12 }, [def("claude-code", "bedrock-prod")]);
    renderList([CLAUDE]);
    const r = await screen.findByTestId("model-provider-bedrock-prod");
    expect(within(r).getByText("Bedrock (prod)")).toBeInTheDocument();
    expect(within(r).getByText("Amazon Bedrock")).toBeInTheDocument();
    expect(within(r).getByText(M.PROVIDES.SSO)).toBeInTheDocument();
    expect(within(r).getByText("Used by Claude Code")).toBeInTheDocument();
    expect(within(r).getByText("Connected by 12 people")).toBeInTheDocument();
    expect(within(r).queryByText(/Default for/)).toBeNull();
  });

  it("the count comes from connected[id]: 1 person, and 0 (or absent) is 'No one has connected yet'", async () => {
    given([bedrock, key, sub], { "bedrock-prod": 1, "anthropic-key": 0 });
    renderList();
    await screen.findByTestId("model-provider-bedrock-prod");
    expect(row("bedrock-prod").getByText("Connected by 1 person")).toBeInTheDocument();
    expect(row("anthropic-key").getByText("No one has connected yet")).toBeInTheDocument();
    expect(row("claude-sub").getByText("No one has connected yet")).toBeInTheDocument();
  });

  it("A3: several for Claude Code — the default chip on one, each kind's line, a name equal to its kind shown once", async () => {
    given([gateway, key, sub], { "corp-gateway": 9, "anthropic-key": 1 }, [def("claude-code", "corp-gateway")]);
    renderList();
    await screen.findByTestId("model-provider-corp-gateway");
    expect(row("corp-gateway").getByText("Your own endpoint")).toBeInTheDocument();
    expect(row("corp-gateway").getByText(M.PROVIDES.TOKEN)).toBeInTheDocument();
    expect(row("corp-gateway").getByText("Default for Claude Code")).toBeInTheDocument();
    expect(row("corp-gateway").getByText("Used by Claude Code, Codex CLI")).toBeInTheDocument();
    expect(row("anthropic-key").getAllByText("Anthropic API key")).toHaveLength(1);
    expect(row("anthropic-key").getByText(M.PROVIDES.KEY)).toBeInTheDocument();
    expect(row("anthropic-key").queryByText(/Default for/)).toBeNull();
    expect(row("claude-sub").getAllByText("Claude subscription")).toHaveLength(1);
    expect(row("claude-sub").getByText(PROVIDER_EDITOR.PROVIDES_CLAUDE)).toBeInTheDocument();
  });

  it("A4 variant: the default for both agents is one chip", async () => {
    given([gateway, key, openai], {}, [def("claude-code", "corp-gateway"), def("codex-cli", "corp-gateway")]);
    renderList();
    await screen.findByTestId("model-provider-corp-gateway");
    expect(row("corp-gateway").getByText("Default for Claude Code and Codex CLI")).toBeInTheDocument();
  });

  it("A7: an off provider says runs can't choose it, and shows Off instead of its count", async () => {
    given([{ ...key, disabled: true }, bedrock], { "anthropic-key": 4 });
    renderList();
    await screen.findByTestId("model-provider-anthropic-key");
    expect(row("anthropic-key").getByText(M.OFF_LINE)).toBeInTheDocument();
    expect(row("anthropic-key").getByText(M.CHIP_OFF)).toBeInTheDocument();
    expect(row("anthropic-key").queryByText(/Connected by|No one has connected/)).toBeNull();
    expect(screen.queryByText(/still the default/)).toBeNull();
  });

  it("A7: off while still a default keeps its chips and says those runs are refused", async () => {
    given([{ ...gateway, disabled: true }, key], {}, [def("claude-code", "corp-gateway")]);
    renderList();
    await screen.findByTestId("model-provider-corp-gateway");
    expect(row("corp-gateway").getByText("Default for Claude Code")).toBeInTheDocument();
    expect(row("corp-gateway").getByText(M.CHIP_OFF)).toBeInTheDocument();
    expect(row("corp-gateway").getByText("Used by Claude Code, Codex CLI")).toBeInTheDocument();
    expect(screen.getByText(M.OFF_STILL_DEFAULT("Claude Code"))).toBeInTheDocument();
  });

  it("A8: a provider no agent uses says so", async () => {
    given([bedrock, { ...openai, harnesses: [] }], { "bedrock-prod": 3 });
    renderList();
    await screen.findByTestId("model-provider-openai-key");
    expect(row("openai-key").getByText(M.UNUSED)).toBeInTheDocument();
    expect(row("openai-key").getByText("No one has connected yet")).toBeInTheDocument();
  });

  it("A9: an agent turned on with no provider gets a notice; 'none' and an unknown enabled state don't", async () => {
    given([bedrock], { "bedrock-prod": 12 });
    renderList([CLAUDE, CODEX, NONE, { ...CODEX, id: "old", display: "Old", enabled: undefined }]);
    expect(await screen.findByText(M.HARNESS_UNSERVED("Codex CLI"))).toBeInTheDocument();
    expect(screen.queryByText(M.HARNESS_UNSERVED("Your own tools"))).toBeNull();
    expect(screen.queryByText(M.HARNESS_UNSERVED("Old"))).toBeNull();
    expect(screen.queryByText(M.HARNESS_UNSERVED("Claude Code"))).toBeNull();
  });

  it("loading, then a failed read with Retry that reads again", async () => {
    getModelProvidersMock.mockReturnValue(new Promise(() => {}));
    getAgentProvidersMock.mockResolvedValue({ providers: {}, etag: null });
    const { container, unmount } = renderList();
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
    expect(screen.queryByText(M.EMPTY_TITLE)).toBeNull();
    expect(screen.queryByText(M.FETCH_FAILED_TITLE)).toBeNull();
    unmount();

    getModelProvidersMock.mockRejectedValueOnce(new Error("boom"));
    renderList();
    expect(await screen.findByText(M.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(M.FETCH_FAILED_BODY)).toBeInTheDocument();
    given([]);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText(M.EMPTY_TITLE)).toBeInTheDocument();
  });
});
