/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents tab (§5c.4/§9.1): rows from SetupStatus.harnesses, `per_user`
// disabled off `bedrock_sso`, and the signed-in admin's own model-access chip
// (claude-code only — modelAccessAgent, internal/api).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { AGENTS, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { AgentsTab } from "./agents-tab";

const getAgentProvidersMock = vi.fn();
const putAgentProvidersMock = vi.fn();
vi.mock("../../../lib/api/agent-providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/agent-providers")>(
    "../../../lib/api/agent-providers",
  );
  return {
    ...actual,
    agentProviders: {
      getAgentProviders: (...a: unknown[]) => getAgentProvidersMock(...a),
      putAgentProviders: (...a: unknown[]) => putAgentProvidersMock(...a),
    },
  };
});

function harness(overrides: Partial<SetupHarnessTool> = {}): SetupHarnessTool {
  return {
    id: "claude-code",
    display: "Claude Code",
    has_gateway: true,
    has_login: true,
    enabled: true,
    ...overrides,
  };
}

const HARNESSES: SetupHarnessTool[] = [
  harness(),
  harness({ id: "codex-cli", display: "Codex CLI" }),
  harness({ id: "none", display: "Your own tools", no_managed_auth: true, has_gateway: false, has_login: false }),
];

beforeEach(() => {
  getAgentProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: '"e0"' });
  putAgentProvidersMock.mockReset();
});

describe("AgentsTab", () => {
  it("renders one row per harness from the roster", async () => {
    render(<AgentsTab harnesses={HARNESSES} operator />);
    expect(await screen.findByTestId("agent-row-claude-code")).toBeInTheDocument();
    expect(screen.getByTestId("agent-row-codex-cli")).toBeInTheDocument();
    expect(screen.getByTestId("agent-row-none")).toBeInTheDocument();
  });

  it("Per person is disabled off bedrock_sso, with its reason", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "anthropic_subscription" }] },
      etag: '"e1"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const perUser = within(row).getByRole("button", { name: AGENTS.SOURCE_PER_USER });
    expect(perUser).toBeDisabled();
    expect(within(row).getByText(AGENTS.PER_USER_UNAVAILABLE)).toBeInTheDocument();
  });

  it("Per person is enabled once bedrock_sso is the selected mechanism", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"e2"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByRole("button", { name: AGENTS.SOURCE_PER_USER })).not.toBeDisabled();
  });

  it("switching the mechanism away from bedrock_sso clears an existing per_user source", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"e2b"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e2c"' });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("radio", { name: /Claude subscription/ }));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const saved = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(saved.credential_source).toBeUndefined();
  });

  it("the signed-in admin's own model-access chip renders on the claude-code row only", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"e3"',
    });
    const modelAccess: SetupModelAccess = { state: "live" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} />);
    const claudeRow = await screen.findByTestId("agent-row-claude-code");
    expect(within(claudeRow).getByText(AGENTS.MODEL_ACCESS_LIVE)).toBeInTheDocument();
    const codexRow = screen.getByTestId("agent-row-codex-cli");
    expect(within(codexRow).queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
  });

  it("saves the whole roster, including untouched catalog rows seeded with a valid mechanism", async () => {
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e5"' });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putAgentProvidersMock).toHaveBeenCalled();
    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.map((a: { id: string }) => a.id).sort()).toEqual(["claude-code", "codex-cli", "none"]);
    expect(body.agents.every((a: { mechanism: string }) => !!a.mechanism)).toBe(true);
    // A lanes-catalog row never seeds "none" (agent400NeedsLane) — only the
    // no-managed-auth row does.
    const none = body.agents.find((a: { id: string }) => a.id === "none");
    expect(none.mechanism).toBe("none");
  });

  it("a disabled row renders disabled with its reason, never hidden", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "codex-cli", mechanism: "openai_api_key", disabled: true }] },
      etag: '"e6"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    const row = await screen.findByTestId("agent-row-codex-cli");
    expect(within(row).getByText(AGENTS.AGENT_ROW_DISABLED_HINT)).toBeInTheDocument();
  });

  it("a custom (non-catalog) row already stored is preserved on save, never edited here", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "my-agent", mechanism: "none" }] },
      etag: '"e7"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e8"' });
    render(<AgentsTab harnesses={HARNESSES} operator />);
    await screen.findByTestId("agent-row-claude-code");
    expect(screen.queryByTestId("agent-row-my-agent")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.some((a: { id: string }) => a.id === "my-agent")).toBe(true);
  });
});
