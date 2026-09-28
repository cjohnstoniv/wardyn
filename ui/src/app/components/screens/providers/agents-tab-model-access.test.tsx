/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #158: not_applicable's own model-access chip, split out of agents-tab.test.tsx.
// The basic "chip + note render, no CTA" case lives in
// agents-tab-per-user.test.tsx; this file covers the chip's own text and its
// claude-code scoping.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { AGENTS } from "../../../lib/workspace-providers-copy";
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
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: {
    getModelProviders: () => Promise.resolve({ providers: {}, connected: {}, etag: null }),
  },
}));

function harness(overrides: Partial<SetupHarnessTool> = {}): SetupHarnessTool {
  return { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, ...overrides };
}

const HARNESSES: SetupHarnessTool[] = [harness()];
const retryRosterMock = vi.fn();
const statusRefreshMock = vi.fn();

beforeEach(() => {
  getAgentProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: '"e0"' });
  putAgentProvidersMock.mockReset();
  retryRosterMock.mockReset();
  statusRefreshMock.mockReset();
});

describe("AgentsTab — not_applicable's own chip text", () => {
  it("renders AGENTS.MODEL_ACCESS_NOT_APPLICABLE, not one of the other five labels", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"nachip1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
    expect(within(row).queryByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED)).not.toBeInTheDocument();
    expect(within(row).queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
  });

  // The server sends no `action` for not_applicable (internal/api/modelaccess.go's
  // modelAccessAction default arm) — the action line renders nothing extra to
  // contradict the chip's own neutral claim.
  it("renders no action line", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"nachip2"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable", action: "" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
    expect(within(row).queryByText(/^Sign in again before/)).not.toBeInTheDocument();
  });
});

// The row's own claude-code scoping (modelAccess is server-scoped to that
// one harness) — not_applicable on a NON-claude-code row must still render
// nothing at all, same as every other state.
describe("AgentsTab — not_applicable is still claude-code only", () => {
  it("a codex-cli row renders no model-access block at all", async () => {
    const CODEX_HARNESSES: SetupHarnessTool[] = [harness({ id: "codex-cli", display: "Codex CLI" })];
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "codex-cli", mechanism: "anthropic_api_key" }] },
      etag: '"nacodex1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable" };
    render(
      <AgentsTab harnesses={CODEX_HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />,
    );
    const row = await screen.findByTestId("agent-row-codex-cli");
    expect(within(row).queryByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).not.toBeInTheDocument();
    expect(within(row).queryByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).not.toBeInTheDocument();
  });
});
