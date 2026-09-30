/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split out of agents-tab.test.tsx (#195): the post-save status refresh and
// the agentCapabilityFor / harness.go catalog parity gate — while the roster/default-provider/ETag describes stay in
// agents-tab.test.tsx.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupHarnessTool } from "../../../lib/types";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
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
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: {
    getModelProviders: () => Promise.resolve({ providers: {}, connected: {}, etag: null }),
  },
}));

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

// Appendix A finding 4 (staleness root cause): harnesses come from the
// PARENT's /setup/status and are never re-read after a successful Save on
// their own — the tab would otherwise go on showing the stale pre-save state
// until an unrelated navigation happens to re-fetch it.
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

// PARITY GATE (widget-registry.parity.test.ts's shape): agentCapabilityFor is a
// hand-typed fold from harness id -> AiCapability, and the other half of it is
// internal/api/harness.go's catalog. A third Gateway-bearing harness added
// there with no entry here loses its impossible-pair check in the model
// provider editor (model-provider-draft.ts).
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
