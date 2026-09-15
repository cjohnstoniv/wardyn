/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents tab (§5c.4/§9.1): rows from SetupStatus.harnesses, `per_user`
// disabled off `bedrock_sso`, and the signed-in admin's own model-access chip
// (claude-code only — modelAccessAgent, internal/api).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { AGENTS, AGENTS_DRAFT, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { HttpError } from "../../../lib/api/core";
import { AgentsTab, agentCapabilityFor } from "./agents-tab";

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

// U-03: the SERVER's settled row (mechanism/credential_source from
// /setup/status) actually saved as bedrock_sso + per_user — as opposed to
// HARNESSES above, whose claude-code row carries neither and stands in for
// "nothing saved yet", however the DRAFT (getAgentProvidersMock) is set.
const HARNESSES_PER_USER_SAVED: SetupHarnessTool[] = [
  harness({ mechanism: "bedrock_sso", credential_source: "per_user" }),
  harness({ id: "codex-cli", display: "Codex CLI" }),
  harness({ id: "none", display: "Your own tools", no_managed_auth: true, has_gateway: false, has_login: false }),
];

// The PARENT's /setup/status re-read — what the roster-unknown Retry must fire
// (the tab's own load() re-reads /agent-providers, which is not that read).
const retryRosterMock = vi.fn();
// A-01: the PARENT's status-only refresh — never `load()` (retryRosterMock
// above stays reserved for the roster-unknown Retry, which has no draft to
// lose). save() must call THIS on success, never retryRosterMock.
const statusRefreshMock = vi.fn();

beforeEach(() => {
  getAgentProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: '"e0"' });
  putAgentProvidersMock.mockReset();
  retryRosterMock.mockReset();
  statusRefreshMock.mockReset();
});

describe("AgentsTab", () => {
  it("renders one row per harness from the roster", async () => {
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    expect(await screen.findByTestId("agent-row-claude-code")).toBeInTheDocument();
    expect(screen.getByTestId("agent-row-codex-cli")).toBeInTheDocument();
    expect(screen.getByTestId("agent-row-none")).toBeInTheDocument();
  });

  it("Per person is disabled off bedrock_sso, with its reason", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "anthropic_subscription" }] },
      etag: '"e1"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const perUser = within(row).getByRole("radio", { name: AGENTS.SOURCE_PER_USER });
    expect(perUser).toBeDisabled();
    expect(within(row).getByText(AGENTS.PER_USER_UNAVAILABLE)).toBeInTheDocument();
  });

  // V2/F3: the credential source was two bare <button type=button> — no group
  // role, no checked state, selection conveyed by the `variant` styling alone —
  // sitting directly under a mechanism control that IS a radiogroup.
  it("the credential source is a radiogroup whose chosen option reads as checked", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"e3"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const group = within(row).getByRole("radiogroup", { name: AGENTS.FIELD_SOURCE });
    expect(within(group).getByRole("radio", { name: AGENTS.SOURCE_PER_USER, checked: true })).toBeInTheDocument();
    expect(within(group).getByRole("radio", { name: AGENTS.SOURCE_SHARED, checked: false })).toBeInTheDocument();
  });

  it("Per person is enabled once bedrock_sso is the selected mechanism", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"e2"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByRole("radio", { name: AGENTS.SOURCE_PER_USER })).not.toBeDisabled();
  });

  // VL-23: the start-URL input is reachable by its Field label (htmlFor/id).
  it("the SSO start-URL input is reachable by its label on a per_user row", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user", sso_start_url: "https://acme.awsapps.com/start" }],
      },
      etag: '"e-a11y"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const input = await screen.findByLabelText(AGENTS.FIELD_SSO_START_URL);
    expect(input.tagName).toBe("INPUT");
    expect(input).toHaveValue("https://acme.awsapps.com/start");
  });

  it("switching the mechanism away from bedrock_sso clears an existing per_user source", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"e2b"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e2c"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
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
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const claudeRow = await screen.findByTestId("agent-row-claude-code");
    expect(within(claudeRow).getByText(AGENTS.MODEL_ACCESS_LIVE)).toBeInTheDocument();
    const codexRow = screen.getByTestId("agent-row-codex-cli");
    expect(within(codexRow).queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
  });

  it("saves the whole roster, including untouched catalog rows seeded with a valid mechanism", async () => {
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e5"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
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
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-codex-cli");
    expect(within(row).getByText(AGENTS.AGENT_ROW_DISABLED_HINT)).toBeInTheDocument();
    // The chip is its OWN canon key, not the hint sliced at its colon.
    expect(within(row).getByText(AGENTS.AGENT_ROW_DISABLED_CHIP)).toBeInTheDocument();
  });

  it("a custom (non-catalog) row already stored is preserved on save, never edited here", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "my-agent", mechanism: "none" }] },
      etag: '"e7"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"e8"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    expect(screen.queryByTestId("agent-row-my-agent")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.some((a: { id: string }) => a.id === "my-agent")).toBe(true);
  });
});

// The tab's Switch is the SERVER's roster answer, and Save writes what the
// admin can see. `SetupHarnessTool.enabled` is `ok && !row.Disabled`
// (setupHarnessTools, internal/api) — the same field agent-picker.tsx:31
// already reads. This tab did not read it: the Switch derived from
// `!row.disabled` on a row resolvedRow INVENTED for every catalog id with no
// stored row, so an admin who had narrowed the roster (CLI/MDM/PUT
// /site-config) and then edited anything here silently re-enabled every catalog
// agent — the mirror image of the `{agents: []}` wipe, in the same function.
describe("AgentsTab — the roster on screen is the server's, and Save writes it", () => {
  const NARROWED: SetupHarnessTool[] = [
    harness(),
    harness({ id: "codex-cli", display: "Codex CLI", enabled: false }),
    harness({ id: "none", display: "Your own tools", no_managed_auth: true, has_gateway: false, has_login: false, enabled: false }),
  ];

  it("an agent the server does not offer renders OFF, and an unrelated Save never adds it", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"n1"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"n2"' });
    render(<AgentsTab harnesses={NARROWED} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);

    const codex = await screen.findByTestId("agent-row-codex-cli");
    expect(within(codex).getByRole("switch")).toHaveAttribute("aria-checked", "false");
    expect(within(screen.getByTestId("agent-row-claude-code")).getByRole("switch")).toHaveAttribute("aria-checked", "true");

    // An edit to a DIFFERENT row, then Save.
    const claude = screen.getByTestId("agent-row-claude-code");
    await userEvent.click(within(claude).getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER }));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.map((a: { id: string }) => a.id)).toEqual(["claude-code"]);
    expect(body.agents[0].mechanism).toBe("bedrock_bearer");
  });

  it("a stored disabled row stays disabled through a Save, never dropped and never re-enabled", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [
          { id: "claude-code", mechanism: "bedrock_sso" },
          { id: "codex-cli", mechanism: "openai_api_key", disabled: true },
        ],
      },
      etag: '"n3"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"n4"' });
    render(<AgentsTab harnesses={NARROWED} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    const [body] = putAgentProvidersMock.mock.calls[0];
    const codex = body.agents.find((a: { id: string }) => a.id === "codex-cli");
    expect(codex).toEqual({ id: "codex-cli", mechanism: "openai_api_key", disabled: true });
  });

  it("legacy open mode (every harness enabled) still pre-fills all-on, in catalog order", async () => {
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"n5"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    // An edit to the FIRST row must not move it to the end of the body.
    await userEvent.click(within(row).getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO }));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.map((a: { id: string }) => a.id)).toEqual(["claude-code", "codex-cli", "none"]);
    expect(body.agents.every((a: { disabled?: boolean }) => !a.disabled)).toBe(true);
  });

  it("turning an off agent on adds exactly that row", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"n6"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"n7"' });
    render(<AgentsTab harnesses={NARROWED} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const codex = await screen.findByTestId("agent-row-codex-cli");
    await userEvent.click(within(codex).getByRole("switch"));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    const [body] = putAgentProvidersMock.mock.calls[0];
    expect(body.agents.map((a: { id: string }) => a.id)).toEqual(["claude-code", "codex-cli"]);
    expect(body.agents.find((a: { id: string }) => a.id === "codex-cli").disabled).toBeFalsy();
  });
});

// SetupStatus.harnesses is OPTIONAL on the wire ("older daemons omit it — treat
// absent as unknown, never as false"). save() builds the whole PUT body from
// this prop, so an absent roster defaulted to [] PUT `{agents: []}` — after
// which every catalog agent reads disabled and every run naming an agent is
// refused, from one click on a Save the admin had no reason to distrust.
describe("AgentsTab — an unknown roster is never a saveable empty one", () => {
  it("renders the fetch-failed state, with no rows and no Save, when the roster is undefined", async () => {
    render(<AgentsTab operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    expect(await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.FETCH_FAILED_BODY)).toBeInTheDocument();
    // The CANON key, not a literal: providers-screen.tsx renders the same
    // control from ACCESS_STATE.FETCH_FAILED_RETRY, and byte-identical today is
    // one canon edit from two spellings.
    expect(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY })).toBeInTheDocument();
    expect(ACCESS_STATE.FETCH_FAILED_RETRY).toBe("Retry");
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeNull();
    expect(screen.queryByTestId("agent-row-claude-code")).toBeNull();
    // The load-bearing half: with no Save there is no path to a PUT at all.
    expect(putAgentProvidersMock).not.toHaveBeenCalled();
  });

  // V1 r2 HIGH: the roster comes from the PARENT's /setup/status read, and this
  // Retry called the tab's own load() — a re-read of /agent-providers, which had
  // not failed. Three clicks, three getAgentProviders calls, nothing moved.
  it("its Retry re-fires the PARENT's roster read, never this tab's own fetch", async () => {
    render(<AgentsTab operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE);
    const agentReads = getAgentProvidersMock.mock.calls.length;
    await userEvent.click(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY }));
    expect(retryRosterMock).toHaveBeenCalledTimes(1);
    expect(getAgentProvidersMock.mock.calls.length).toBe(agentReads);
  });

  // A daemon that genuinely reports zero agents is a DIFFERENT case: the tab
  // still reads (its lead renders), there are simply no rows. Save stays
  // withheld because the only body it could write is `{agents: []}` — which is
  // the same wipe, just reached honestly.
  it("renders the lead with no rows and no Save when the roster is reported empty", async () => {
    render(<AgentsTab harnesses={[]} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    expect(await screen.findByText(AGENTS.AGENTS_LEAD)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeNull();
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeNull();
    expect(putAgentProvidersMock).not.toHaveBeenCalled();
  });
});

// V1 r2 MEDIUM: `per_user` is captured for an AWS SSO sign-in only, so a stored
// row pairing it with any other mechanism rendered "Per person" ACTIVE and
// DISABLED at once, hid the start-URL field, and re-PUT both fields verbatim on
// the next unrelated Save — a value the admin could neither see nor clear.
describe("AgentsTab — a stored per_user on a mechanism that can't carry it is normalised on LOAD", () => {
  const STORED = {
    id: "claude-code",
    mechanism: "anthropic_api_key",
    credential_source: "per_user",
    sso_start_url: "https://acme.awsapps.com/start",
  };

  it("renders Shared, with no start-URL field and no active-and-disabled Per person", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [STORED] }, etag: '"p1"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const shared = within(row).getByRole("radio", { name: AGENTS.SOURCE_SHARED });
    const perUser = within(row).getByRole("radio", { name: AGENTS.SOURCE_PER_USER });
    // "Active" is the secondary variant the tab paints the chosen source with.
    expect(shared.className.split(/\s+/)).toContain("bg-secondary");
    expect(perUser.className.split(/\s+/)).not.toContain("bg-secondary");
    expect(perUser).toBeDisabled();
    expect(within(row).queryByLabelText(AGENTS.FIELD_SSO_START_URL)).toBeNull();
    // Nor is the hidden value readable anywhere on the surface.
    expect(screen.queryByDisplayValue("https://acme.awsapps.com/start")).toBeNull();
  });

  it("...and the next Save PUTs the normalised row, not the pair it loaded", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [STORED] }, etag: '"p2"' });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"p3"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    const [body] = putAgentProvidersMock.mock.calls[0];
    const claude = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(claude.mechanism).toBe("anthropic_api_key");
    expect(claude.credential_source ?? "shared").toBe("shared");
    expect(claude.sso_start_url).toBeUndefined();
  });

  it("a bedrock_sso row keeps both fields — that pair IS valid", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user", sso_start_url: "https://acme.awsapps.com/start" }],
      },
      etag: '"p4"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByLabelText(AGENTS.FIELD_SSO_START_URL)).toHaveValue("https://acme.awsapps.com/start");
  });

  // A-09 (review fix-first): a bedrock_sso row with credential_source SHARED
  // (or unset) is the same "the admin can neither see nor clear it" trap as
  // the mechanism case above — the pin fields aren't shown (per_user gate),
  // so a stale sso_account_id/sso_role_name/sso_start_url from another client
  // survived load-normalise and rode the next unrelated Save straight into
  // agent400SSOPinUnused. One clause (credential_source !== "per_user") now
  // clears all four fields together, regardless of mechanism.
  it("a stored bedrock_sso+SHARED row carrying a stale pin is normalised on LOAD too", async () => {
    const STORED_SHARED_WITH_PIN = {
      id: "claude-code",
      mechanism: "bedrock_sso",
      sso_start_url: "https://acme.awsapps.com/start",
      sso_account_id: "111111111111",
      sso_role_name: "BedrockRunner",
    };
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [STORED_SHARED_WITH_PIN] }, etag: '"p5"' });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"p6"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    // Hidden on load: the row renders Shared, so none of the three fields
    // (per_user-gated) show, and the stale values are not readable anywhere.
    expect(within(row).queryByLabelText(AGENTS.FIELD_SSO_START_URL)).toBeNull();
    expect(within(row).queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toBeNull();
    expect(within(row).queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME)).toBeNull();
    expect(screen.queryByDisplayValue("111111111111")).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const claude = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(claude.credential_source ?? "shared").toBe("shared");
    expect(claude.sso_start_url).toBeUndefined();
    expect(claude.sso_account_id).toBeUndefined();
    expect(claude.sso_role_name).toBeUndefined();
  });
});

// V1 r2 LOW: the chip's final `else` painted MODEL_ACCESS_NOT_CONFIGURED over
// ANY unrecognised state, so a daemon reporting `expired_renewable` — a live,
// renewable credential — told the admin they were signed out, with no CTA.
describe("AgentsTab — a model-access state outside the five gets no chip", () => {
  it("renders NO chip and no sign-in button for expired_renewable", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] }, etag: '"m1"' });
    const modelAccess = { state: "expired_renewable" } as SetupModelAccess;
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED)).toBeNull();
    expect(within(row).queryByText(AGENTS.MODEL_ACCESS_LIVE)).toBeNull();
    expect(within(row).queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
    // The note beside the chip is about the ADMIN's own credential and still reads.
    expect(within(row).getByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).toBeInTheDocument();
  });

  it("the five known states still render their own chip", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] }, etag: '"m2"' });
    const modelAccess: SetupModelAccess = { state: "shared_expired" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByText(AGENTS.MODEL_ACCESS_SHARED_EXPIRED)).toBeInTheDocument();
  });
});

// V1 r2 LOW: the admin's own sign-in under a per_user row goes against the ROW's
// stored portal — the server ignores a typed one — so the pane must not ask.
describe("AgentsTab — the admin's per_user sign-in never asks for the portal", () => {
  it("opens the login pane with the managed note instead of the start-URL field", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user", sso_start_url: "https://acme.awsapps.com/start" }] },
      etag: '"s1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_configured" };
    render(<AgentsTab harnesses={HARNESSES_PER_USER_SAVED} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    expect(await screen.findByText(AGENTS.SSO_START_URL_MANAGED)).toBeInTheDocument();
    // The PANE's own input, by id: the row's stored start-URL field shares the
    // frozen label (FIELD_SSO_START_URL), so a label query matches either.
    expect(document.getElementById("harness-login-start-url")).toBeNull();
  });

  it("a SHARED row's sign-in still asks — nothing is stored to use", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"s2"',
    });
    const modelAccess: SetupModelAccess = { state: "not_configured" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await screen.findByTestId("login-start-url-prompt");
    expect(document.getElementById("harness-login-start-url")).toBeInTheDocument();
    expect(screen.queryByText(AGENTS.SSO_START_URL_MANAGED)).toBeNull();
  });

  // U-03: per_user toggled in the DRAFT but not yet saved (the server's
  // settled row, `harnesses`, still reads shared) — the pane must still ask
  // for the start URL. Suppressing the prompt here is exactly the shape
  // that leads to a 400 with no field to answer (harnesscred.go:761 reads
  // the STORED row, which has no start URL of its own to fall back to).
  it("a DRAFTED (unsaved) per_user row's sign-in still asks — the server hasn't saved a portal yet", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"s3"',
    });
    const modelAccess: SetupModelAccess = { state: "not_configured" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await screen.findByTestId("login-start-url-prompt");
    expect(document.getElementById("harness-login-start-url")).toBeInTheDocument();
    expect(screen.queryByText(AGENTS.SSO_START_URL_MANAGED)).toBeNull();
  });
});

// The concurrency + refusal contract. The tab reads an ETag on GET and must
// send it back on PUT, or two admins silently overwrite each other; a 412 is
// the server saying someone already did, and a 400 is the server's own words
// about a body it will not take.
describe("AgentsTab — the ETag / 412 / 400 contract", () => {
  it("hands the GET's ETag to the PUT, and the client sends it as If-Match", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"v7"' });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"v8"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await waitFor(() => expect(putAgentProvidersMock).toHaveBeenCalled());
    expect(putAgentProvidersMock.mock.calls[0][1]).toBe('"v7"');

    // ...and the other half of that sentence, over the REAL client (the module
    // is mocked for this file, so the actual one is imported here): the value
    // the tab passes is what lands on the wire as If-Match. Split in two
    // because the tab cannot see a header and the client cannot see a tab.
    const actual = await vi.importActual<typeof import("../../../lib/api/agent-providers")>(
      "../../../lib/api/agent-providers",
    );
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response("{}", { status: 200, headers: { ETag: '"v8"' } }));
    try {
      await actual.agentProviders.putAgentProviders({ agents: [] }, '"v7"');
      const init = fetchSpy.mock.calls[0][1] as RequestInit;
      expect(new Headers(init.headers).get("If-Match")).toBe('"v7"');
    } finally {
      fetchSpy.mockRestore();
    }
  });

  it("a 412 renders SAVED_ELSEWHERE and overwrites nothing", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"v1"',
    });
    putAgentProvidersMock.mockRejectedValue(new HttpError(412, "precondition failed"));
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.SAVED_ELSEWHERE_BODY)).toBeInTheDocument();
    // The rows come down with the banner: there is nothing to save over.
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeNull();
    expect(putAgentProvidersMock).toHaveBeenCalledTimes(1);
  });

  it("a 400 renders SAVE_REFUSED_TITLE over the server's verbatim body", async () => {
    const refusal = 'agents[0]: mechanism "bedrock_sso" requires an sso_start_url';
    putAgentProvidersMock.mockRejectedValue(new HttpError(400, refusal));
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    // VERBATIM — never a console reword of the server's own sentence.
    expect(screen.getByText(refusal)).toBeInTheDocument();
    // Still saveable: a refused body is a fixable one, unlike a 412.
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeInTheDocument();
  });
});

// V1 r2 HIGH (Appendix A finding 4, staleness root cause): modelAccess comes
// from the PARENT's /setup/status, never re-read after a successful Save, so
// right after declaring per_user the admin's own door can go on showing the
// stale pre-save state until an unrelated navigation happens to re-fetch it.
//
// A-01 (review fix-first): the re-fire is `onStatusRefresh` — /setup/status
// ONLY — never `onRetryRoster`, which is the PARENT's whole load(): it also
// resets the Git/Storage tabs' own unsaved `draft`, clears a pending 412
// banner, and a transient GET failure there would flip the whole screen to
// FETCH_FAILED right after a successful, unrelated agent save.
describe("AgentsTab — a successful Save re-fires ONLY the parent's status read", () => {
  it("calls onStatusRefresh after a successful save, never onRetryRoster", async () => {
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"r2"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await waitFor(() => expect(putAgentProvidersMock).toHaveBeenCalled());
    expect(statusRefreshMock).toHaveBeenCalledTimes(1);
    expect(retryRosterMock).not.toHaveBeenCalled();
  });

  it("does NOT call onStatusRefresh when the save is refused (400)", async () => {
    putAgentProvidersMock.mockRejectedValue(new HttpError(400, "refused"));
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE);
    expect(statusRefreshMock).not.toHaveBeenCalled();
    expect(retryRosterMock).not.toHaveBeenCalled();
  });

  // A-05: the 412 twin — no negative control existed for this call site.
  it("does NOT call onStatusRefresh when someone else saved first (412)", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"r3"',
    });
    putAgentProvidersMock.mockRejectedValue(new HttpError(412, "precondition failed"));
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await screen.findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE);
    expect(statusRefreshMock).not.toHaveBeenCalled();
    expect(retryRosterMock).not.toHaveBeenCalled();
  });
});

// Appendix A finding 4: the per_user sign-in affordance moves to the TOP of
// the expanded claude-code row, in a tinted role="status" panel, when this
// row is credential_source per_user AND the state is one of the three
// actionable ones — the legacy Settings door stops being the one an admin
// reaches for.
describe("AgentsTab — the per_user sign-in banner", () => {
  const PER_USER_ROW = { id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" };

  it("renders the prominent banner above the mechanism field for an actionable state", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [PER_USER_ROW] }, etag: '"b1"' });
    const modelAccess: SetupModelAccess = { state: "not_configured", action: "Sign in to AWS" };
    render(<AgentsTab harnesses={HARNESSES_PER_USER_SAVED} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    const banner = within(row).getByTestId("per-user-sign-in-banner");
    expect(within(banner).getByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).toBeInTheDocument();
    expect(within(banner).getByText(AGENTS_DRAFT.PER_USER_SIGN_IN_BODY)).toBeInTheDocument();
    expect(within(banner).getByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).toBeInTheDocument();
    expect(within(banner).getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    // A-07: role="status" covers ONLY the title/body pair — never
    // ModelAccessSignIn (HarnessLoginPane's own multi-step device-code/poll
    // flow would otherwise re-announce wholesale on every poll tick).
    const statusRegion = within(banner).getByRole("status");
    expect(within(statusRegion).getByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).toBeInTheDocument();
    expect(within(statusRegion).queryByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).not.toBeInTheDocument();
    expect(within(statusRegion).queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    // It renders BEFORE the mechanism radiogroup — prominence, not an addition.
    const mechanismField = within(row).getByRole("radiogroup", { name: AGENTS.FIELD_MECHANISM });
    expect(banner.compareDocumentPosition(mechanismField) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("a shared row (negative control) never renders the banner", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"b2"',
    });
    const modelAccess: SetupModelAccess = { state: "not_configured", action: "Sign in to AWS" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).not.toBeInTheDocument();
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
    // The chip still renders, just in its ordinary spot at the bottom.
    expect(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
  });

  it("a per_user row that is already live never renders the banner", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [PER_USER_ROW] }, etag: '"b3"' });
    const modelAccess: SetupModelAccess = { state: "live" };
    render(<AgentsTab harnesses={HARNESSES_PER_USER_SAVED} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).not.toBeInTheDocument();
  });

  // U-03 (blind review lens-U): a per_user row that is only DRAFTED — Per
  // person toggled in the picker, not yet saved — must not surface the live
  // CTA. Its only outcome would be a server 400 with no field to answer
  // (harnesscred.go's 400 fires on the STORED row's start URL being empty,
  // which is exactly the shared-server state here). `harnesses` (the server
  // read) still says shared/unsaved even though the draft says per_user.
  it("a per_user row that is only DRAFTED (not yet saved) never renders the banner", async () => {
    getAgentProvidersMock.mockResolvedValue({ providers: { agents: [PER_USER_ROW] }, etag: '"b4"' });
    const modelAccess: SetupModelAccess = { state: "not_configured", action: "Sign in to AWS" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).not.toBeInTheDocument();
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
    // The ordinary bottom chip still renders — the row can still be signed
    // into, just not with prominence it has not earned by being saved.
    expect(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
  });

  // R-01 (review): the row is SAVED disabled + per_user, and the DRAFT
  // switches it back on — perUserSaved must still read `false`. It is
  // `isPerUserSsoRow`'s `enabled !== false` conjunct, not two of its three
  // parts: the server's login predicate (perUserLoginRow) and its
  // model_access scoping (awsSSOScopeFor) both treat a disabled row as NOT
  // per_user, so neither prominence nor a suppressed start-URL prompt is
  // earned by an unsaved switch-on alone.
  it("a DISABLED saved per_user row switched back on in the draft gets no banner and no suppressed prompt", async () => {
    const DISABLED_PER_USER_HARNESSES: SetupHarnessTool[] = [
      harness({ mechanism: "bedrock_sso", credential_source: "per_user", enabled: false }),
      harness({ id: "codex-cli", display: "Codex CLI" }),
      harness({ id: "none", display: "Your own tools", no_managed_auth: true, has_gateway: false, has_login: false }),
    ];
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ ...PER_USER_ROW, disabled: false }] },
      etag: '"b5"',
    });
    const modelAccess: SetupModelAccess = { state: "not_configured", action: "Sign in to AWS" };
    render(
      <AgentsTab
        harnesses={DISABLED_PER_USER_HARNESSES}
        operator
        modelAccess={modelAccess}
        onRetryRoster={retryRosterMock}
        onStatusRefresh={statusRefreshMock}
      />,
    );
    const row = await screen.findByTestId("agent-row-claude-code");
    // The draft's disabled: false wins the Switch — the row body renders.
    expect(within(row).queryByText(AGENTS.AGENT_ROW_DISABLED_HINT)).not.toBeInTheDocument();
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
    await userEvent.click(within(row).getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await screen.findByTestId("login-start-url-prompt");
    expect(document.getElementById("harness-login-start-url")).toBeInTheDocument();
    expect(screen.queryByText(AGENTS.SSO_START_URL_MANAGED)).toBeNull();
  });
});

// Appendix A finding 5, agents-tab half: not_applicable is the admin-token
// principal's own answer and carries no chip label — the whole claude-code
// block (chip + ADMIN_OWN_CHIP_NOTE + sign-in CTA) must not render at all,
// never an empty chip with the note still underneath it.
describe("AgentsTab — not_applicable renders no model-access block at all", () => {
  it("no chip, no ADMIN_OWN_CHIP_NOTE, no sign-in CTA", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"na1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).queryByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).not.toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
  });
});

// Finding 4's other half: the roster pin. Two labelled Inputs mirroring
// sso_start_url — shown only under bedrock_sso + per_user, cleared exactly
// when the start URL is (normalizeAgentRow, and the same onChange paths).
describe("AgentsTab — the roster pin (sso_account_id / sso_role_name)", () => {
  it("renders both pin inputs, reachable by label, only on a bedrock_sso per_user row", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [
          {
            id: "claude-code",
            mechanism: "bedrock_sso",
            credential_source: "per_user",
            sso_start_url: "https://acme.awsapps.com/start",
            sso_account_id: "111111111111",
            sso_role_name: "BedrockRunner",
          },
        ],
      },
      etag: '"pin1"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    const accountInput = await screen.findByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID);
    const roleInput = screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME);
    expect(accountInput).toHaveValue("111111111111");
    expect(roleInput).toHaveValue("BedrockRunner");
  });

  it("hides both pin inputs on a shared row", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"pin2"',
    });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    expect(screen.queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toBeNull();
    expect(screen.queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME)).toBeNull();
  });

  it("saves typed pin values on the row", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"pin3"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"pin4"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.type(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID), "222222222222");
    await userEvent.type(screen.getByLabelText(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME), "DevPower");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const claude = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(claude.sso_account_id).toBe("222222222222");
    expect(claude.sso_role_name).toBe("DevPower");
  });

  it("switching the mechanism away from bedrock_sso clears a stored pin too", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [
          {
            id: "claude-code",
            mechanism: "bedrock_sso",
            credential_source: "per_user",
            sso_start_url: "https://acme.awsapps.com/start",
            sso_account_id: "111111111111",
            sso_role_name: "BedrockRunner",
          },
        ],
      },
      etag: '"pin5"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"pin6"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("radio", { name: /Claude subscription/ }));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const saved = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(saved.sso_account_id).toBeUndefined();
    expect(saved.sso_role_name).toBeUndefined();
  });

  it("switching credential source back to Shared clears a stored pin too", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [
          {
            id: "claude-code",
            mechanism: "bedrock_sso",
            credential_source: "per_user",
            sso_start_url: "https://acme.awsapps.com/start",
            sso_account_id: "111111111111",
            sso_role_name: "BedrockRunner",
          },
        ],
      },
      etag: '"pin7"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"pin8"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(within(row).getByRole("radio", { name: AGENTS.SOURCE_SHARED }));
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const saved = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(saved.sso_account_id).toBeUndefined();
    expect(saved.sso_role_name).toBeUndefined();
  });

  it("a load-time normalise (per_user paired with a non-bedrock_sso mechanism) drops the pin too", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: {
        agents: [
          {
            id: "claude-code",
            mechanism: "anthropic_api_key",
            credential_source: "per_user",
            sso_start_url: "https://acme.awsapps.com/start",
            sso_account_id: "111111111111",
            sso_role_name: "BedrockRunner",
          },
        ],
      },
      etag: '"pin9"',
    });
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"pin10"' });
    render(<AgentsTab harnesses={HARNESSES} operator onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    await screen.findByTestId("agent-row-claude-code");
    expect(screen.queryByLabelText(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const [body] = putAgentProvidersMock.mock.calls[0];
    const saved = body.agents.find((a: { id: string }) => a.id === "claude-code");
    expect(saved.sso_account_id).toBeUndefined();
    expect(saved.sso_role_name).toBeUndefined();
  });
});

// PARITY GATE (widget-registry.parity.test.ts's shape): agentCapabilityFor is a
// hand-typed fold from harness id -> AiCapability, and the other half of it is
// internal/api/harness.go's catalog. A third Gateway-bearing harness added
// there with no entry here loses every impossible-pair reason on its row — the
// radio list renders enabled choices the run will then be refused for.
const GO_REL = "internal/api/harness.go";
function goHarnessFile(): string {
  let dir = process.cwd();
  for (;;) {
    const candidate = resolve(dir, GO_REL);
    if (existsSync(candidate)) return candidate;
    const up = dirname(dir);
    if (up === dir) throw new Error(`harness parity: could not locate ${GO_REL} above ${process.cwd()}`);
    dir = up;
  }
}

/** The catalog's ids, split by whether the row declares a Gateway (a managed
 *  model credential). Throws rather than returning empty sets: a vacuous pass
 *  is what a parity gate exists to prevent. */
function goCatalog(): { gateway: string[]; all: string[] } {
  const src = readFileSync(goHarnessFile(), "utf8");
  const block = /var\s+harnessCatalog\s*=\s*\[\]harnessDef\{([\s\S]*?)\n\}/.exec(src);
  if (!block) {
    throw new Error(
      "harness parity: could not find 'var harnessCatalog = []harnessDef{...}' in " +
        `${GO_REL} — if the declaration was renamed or moved, update this gate in the SAME commit.`,
    );
  }
  const gateway: string[] = [];
  const all: string[] = [];
  let current = "";
  for (const line of block[1].split("\n")) {
    const id = /^\s*ID:\s*"([^"]+)"/.exec(line);
    if (id) {
      current = id[1];
      all.push(current);
      continue;
    }
    if (/^\s*Gateway:\s*&llmProvider\{/.test(line) && current) gateway.push(current);
  }
  if (all.length === 0 || gateway.length === 0) {
    throw new Error(`harness parity: harnessCatalog parsed to an EMPTY set in ${GO_REL}`);
  }
  return { gateway, all };
}

describe("agentCapabilityFor and internal/api/harness.go's catalog", () => {
  it("maps every Gateway-bearing harness id", () => {
    for (const id of goCatalog().gateway) {
      expect(agentCapabilityFor(id), `${id} has a Gateway in ${GO_REL} but no capability here`).toBeTruthy();
    }
  });

  it("maps NOTHING the catalog does not carry a Gateway for", () => {
    const { gateway, all } = goCatalog();
    for (const id of all.filter((i) => !gateway.includes(i))) {
      expect(agentCapabilityFor(id), `${id} has no Gateway in ${GO_REL} but is mapped here`).toBeUndefined();
    }
    // A custom WARDYN_AGENT_IMAGES id is outside the catalog entirely.
    expect(agentCapabilityFor("my-agent")).toBeUndefined();
  });
});
