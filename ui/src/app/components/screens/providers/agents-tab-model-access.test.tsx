/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #158: not_applicable's own model-access chip, split out of agents-tab.test.tsx
// (which sits at the 1000-line file-size cap) per the agents-tab-source-gate.test.tsx
// precedent. The basic "chip + note render, no CTA" case lives in
// agents-tab.test.tsx's own (rewritten) not_applicable describe block; this file
// covers the cases that block doesn't: the chip's own text/tone, and that
// not_applicable never earns the per_user prominent banner even under a saved
// per_user row (it is excluded from MODEL_ACCESS_ACTIONABLE).
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

function harness(overrides: Partial<SetupHarnessTool> = {}): SetupHarnessTool {
  return { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, ...overrides };
}

const HARNESSES: SetupHarnessTool[] = [harness()];
// U-03: the SERVER's settled row — mechanism/credential_source actually
// saved as bedrock_sso + per_user (agents-tab.test.tsx's own precedent for
// this fixture shape).
const HARNESSES_PER_USER_SAVED: SetupHarnessTool[] = [harness({ mechanism: "bedrock_sso", credential_source: "per_user" })];
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

// Prominence (finding 4) is gated on MODEL_ACCESS_ACTIONABLE, which excludes
// not_applicable — a mechanism has nothing actionable to do, so the banner
// must never promote it, even under a SAVED per_user row that would promote
// any of the other five.
describe("AgentsTab — not_applicable never earns the per_user prominent banner", () => {
  it("a saved per_user row under not_applicable keeps the chip at the bottom, no banner", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"naprom1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable" };
    render(
      <AgentsTab
        harnesses={HARNESSES_PER_USER_SAVED}
        operator
        modelAccess={modelAccess}
        onRetryRoster={retryRosterMock}
        onStatusRefresh={statusRefreshMock}
      />,
    );
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
    expect(within(row).getByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
    expect(within(row).getByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).toBeInTheDocument();
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
