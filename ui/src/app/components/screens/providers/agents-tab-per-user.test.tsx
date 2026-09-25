/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split out of agents-tab.test.tsx (#195): that file was over the 800-line
// test gate. The per_user save/sign-in surface stays together here — the
// post-save status refresh, the prominent per_user sign-in banner, the
// not_applicable chip, the roster-pin fields, and the agentCapabilityFor /
// harness.go catalog parity gate — while the roster/mechanism/ETag describes
// stay in agents-tab.test.tsx.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { AGENTS, AGENTS_DRAFT, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { HttpError } from "../../../lib/api/core";
import { AgentsTab, agentCapabilityFor } from "./agents-tab";
import { baseStatus } from "../../../lib/test-fixtures";
import { WithDoor } from "../../../../test/door-harness";

// The tab's sign-in button opens the shell's one door (#544), whose AWS pane
// reads the SHELL's status — the server's settled rows, `harnesses` here.
const withDoor = (ui: JSX.Element, harnesses: SetupHarnessTool[]) => (
  <WithDoor status={baseStatus({ harnesses })}>{ui}</WithDoor>
);

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

// Appendix A finding 4 (staleness root cause): modelAccess comes from the
// PARENT's /setup/status and is never re-read after a successful Save on its
// own — right after declaring per_user, the admin's own door would otherwise
// go on showing the stale pre-save state until an unrelated navigation
// happens to re-fetch it.
//
// A-01: the re-fire must be `onStatusRefresh` — /setup/status ONLY — never
// `onRetryRoster`, which is the PARENT's whole load(): that also resets the
// Git/Storage tabs' own unsaved `draft`, clears a pending 412 banner, and a
// transient GET failure there would flip the whole screen to FETCH_FAILED
// right after a successful, unrelated agent save.
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

  // The 412 twin: a negative control for this call site.
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
    // role="status" covers ONLY the title/body pair — never ModelAccessSignIn
    // (HarnessLoginPane's own multi-step device-code/poll flow would
    // otherwise re-announce wholesale on every poll tick).
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

  // U-03: a per_user row that is only DRAFTED — Per person toggled in the
  // picker, not yet saved — must not surface the live CTA. Its only outcome
  // would be a server 400 with no field to answer
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

  // R-01: the row is SAVED disabled + per_user, and the DRAFT switches it
  // back on — perUserSaved must still read `false`. It is
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
      withDoor(
        <AgentsTab
          harnesses={DISABLED_PER_USER_HARNESSES}
          operator
          modelAccess={modelAccess}
          onRetryRoster={retryRosterMock}
          onStatusRefresh={statusRefreshMock}
        />,
        DISABLED_PER_USER_HARNESSES,
      ),
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

// #158 (Appendix A finding 5, agents-tab half, rewritten): not_applicable now
// carries a real, neutral chip label (MODEL_ACCESS_CHIP_LABEL.not_applicable)
// — the whole claude-code block renders, chip + ADMIN_OWN_CHIP_NOTE, same as
// any other state. It still offers NO sign-in CTA: there is no person here
// to sign in as. Further not_applicable cases (tone, prominence) live in
// agents-tab-model-access.test.tsx (agents-tab.test.tsx sits at the
// file-size cap).
describe("AgentsTab — not_applicable renders its own chip, never a sign-in CTA", () => {
  it("chip + ADMIN_OWN_CHIP_NOTE render, no sign-in CTA, no per-user banner", async () => {
    getAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso" }] },
      etag: '"na1"',
    });
    const modelAccess: SetupModelAccess = { state: "not_applicable" };
    render(<AgentsTab harnesses={HARNESSES} operator modelAccess={modelAccess} onRetryRoster={retryRosterMock} onStatusRefresh={statusRefreshMock} />);
    const row = await screen.findByTestId("agent-row-claude-code");
    expect(within(row).getByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
    expect(within(row).getByText(AGENTS.ADMIN_OWN_CHIP_NOTE)).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    expect(within(row).queryByTestId("per-user-sign-in-banner")).not.toBeInTheDocument();
  });
});

// Appendix A finding 4's other half: the roster pin. Two labelled Inputs
// mirroring sso_start_url — shown only under bedrock_sso + per_user, cleared
// exactly when the start URL is (normalizeAgentRow, and the same onChange
// paths).
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
      // F4-F9: a per_user row needs a valid start URL or Save disables —
      // this test is about the PIN fields, so the row is otherwise valid.
      providers: {
        agents: [
          { id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user", sso_start_url: "https://acme.awsapps.com/start" },
        ],
      },
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
