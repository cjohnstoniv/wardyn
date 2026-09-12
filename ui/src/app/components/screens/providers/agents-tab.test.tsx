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
import { AGENTS, PROVIDERS } from "../../../lib/workspace-providers-copy";
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
    // The chip is its OWN canon key, not the hint sliced at its colon.
    expect(within(row).getByText(AGENTS.AGENT_ROW_DISABLED_CHIP)).toBeInTheDocument();
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

// SetupStatus.harnesses is OPTIONAL on the wire ("older daemons omit it — treat
// absent as unknown, never as false"). save() builds the whole PUT body from
// this prop, so an absent roster defaulted to [] PUT `{agents: []}` — after
// which every catalog agent reads disabled and every run naming an agent is
// refused, from one click on a Save the admin had no reason to distrust.
describe("AgentsTab — an unknown roster is never a saveable empty one", () => {
  it("renders the fetch-failed state, with no rows and no Save, when the roster is undefined", async () => {
    render(<AgentsTab operator />);
    expect(await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.FETCH_FAILED_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeNull();
    expect(screen.queryByTestId("agent-row-claude-code")).toBeNull();
    // The load-bearing half: with no Save there is no path to a PUT at all.
    expect(putAgentProvidersMock).not.toHaveBeenCalled();
  });

  // A daemon that genuinely reports zero agents is a DIFFERENT case: the tab
  // still reads (its lead renders), there are simply no rows. Save stays
  // withheld because the only body it could write is `{agents: []}` — which is
  // the same wipe, just reached honestly.
  it("renders the lead with no rows and no Save when the roster is reported empty", async () => {
    render(<AgentsTab harnesses={[]} operator />);
    expect(await screen.findByText(AGENTS.AGENTS_LEAD)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeNull();
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeNull();
    expect(putAgentProvidersMock).not.toHaveBeenCalled();
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
    render(<AgentsTab harnesses={HARNESSES} operator />);
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
    render(<AgentsTab harnesses={HARNESSES} operator />);
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
    render(<AgentsTab harnesses={HARNESSES} operator />);
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    // VERBATIM — never a console reword of the server's own sentence.
    expect(screen.getByText(refusal)).toBeInTheDocument();
    // Still saveable: a refused body is a fixable one, unlike a 412.
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeInTheDocument();
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
