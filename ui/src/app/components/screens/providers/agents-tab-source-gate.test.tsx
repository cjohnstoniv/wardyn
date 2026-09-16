/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split out of agents-tab.test.tsx (which sits at the 1000-line file-size
// cap): the credential-source radiogroup's roving tabindex (F4-F13) and the
// per_user start-URL Save gate (F4-F9) — both new this round, both scoped to
// the credential-source Field, so they share this file rather than growing
// the main suite past the cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupHarnessTool } from "../../../lib/types";
import { AGENTS, AGENTS_DRAFT, PROVIDERS } from "../../../lib/workspace-providers-copy";
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
const retryRosterMock = vi.fn();
const statusRefreshMock = vi.fn();

beforeEach(() => {
  getAgentProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: '"e0"' });
  putAgentProvidersMock.mockReset();
  retryRosterMock.mockReset();
  statusRefreshMock.mockReset();
});

describe("AgentsTab — the credential-source group has roving tabindex and arrow keys (F4-F13)", () => {
  // F4-F13 (Appendix A V8): the two role="radio" Buttons were each their own
  // Tab stop, and arrow keys did nothing.
  it("only the checked radio is a Tab stop; ArrowRight/ArrowLeft move selection and focus", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"e3b"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const shared = within(row).getByRole("radio", { name: AGENTS.SOURCE_SHARED });
    const perUser = within(row).getByRole("radio", { name: AGENTS.SOURCE_PER_USER });
    expect(shared).toHaveAttribute("tabIndex", "0");
    expect(perUser).toHaveAttribute("tabIndex", "-1");

    shared.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(perUser).toHaveFocus();
    expect(perUser).toHaveAttribute("aria-checked", "true");
    expect(shared).toHaveAttribute("aria-checked", "false");
    expect(perUser).toHaveAttribute("tabIndex", "0");
    expect(shared).toHaveAttribute("tabIndex", "-1");

    await userEvent.keyboard("{ArrowLeft}");
    expect(shared).toHaveFocus();
    expect(shared).toHaveAttribute("aria-checked", "true");
  });
});

// F4-F9 (Appendix A V8): "Per person" on a bedrock_sso row with an empty
// start URL is a guaranteed 400 (agent_providers.go's
// validateAgentCredentialSource) — Save must not stay enabled over it.
describe("AgentsTab — Save is withheld over an invalid per_user start URL (F4-F9)", () => {
  it("Save disables the moment Per person is picked with no start URL, and re-enables on a valid one", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] }, etag: '"e9"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const save = screen.getByRole("button", { name: PROVIDERS.SAVE_CTA });
    expect(save).toBeEnabled();

    await userEvent.click(within(row).getByRole("radio", { name: AGENTS.SOURCE_PER_USER }));
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeDisabled();
    expect(within(row).getByText(AGENTS_DRAFT.SSO_START_URL_REQUIRED)).toBeInTheDocument();

    await userEvent.type(within(row).getByLabelText(AGENTS.FIELD_SSO_START_URL), "https://acme.awsapps.com/start");
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeEnabled();
    expect(within(row).queryByText(AGENTS_DRAFT.SSO_START_URL_REQUIRED)).not.toBeInTheDocument();
  });

  // Negative control: a valid start URL typed straight away keeps Save
  // enabled throughout — the gate never fires on a row that was never
  // invalid.
  it("a row loaded with a valid start URL keeps Save enabled", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user", sso_start_url: "https://acme.awsapps.com/start" }],
      },
      etag: '"e10"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeEnabled();
  });
});
