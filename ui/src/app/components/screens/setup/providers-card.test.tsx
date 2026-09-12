/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ProvidersCard — the user-drives-card.tsx precedent, cloned: one component in
// two homes (the funnel's `providers` step body and Settings' third card,
// replacing Git host). What is pinned here is what the card DECIDES:
//
//   1. it counts ENABLED git-provider rows, never hosts,
//   2. no providers is its own sentence, and the link still goes to /providers,
//   3. a failed read leaves the summary ABSENT rather than a confident empty,
//   4. SUPER only,
//   5. ZERO teal.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/providers")>("../../../lib/api/providers");
  return { ...actual, providers: { ...actual.providers, getWorkspaceProviders: () => getWorkspaceProvidersMock() } };
});

const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});

import type { GitProvider } from "../../../lib/api/providers";
import type { SetupHarnessTool } from "../../../lib/types";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ProvidersCard } from "./providers-card";

const snap = (rows: GitProvider[]) => ({ providers: { git: rows }, etag: '"abc"' });

const row = (id: string, disabled = false): GitProvider => ({
  id,
  kind: "github",
  base_urls: [`https://github.com/${id}`],
  disabled,
});

// `harnesses` is the roster BOTH homes already hold (SetupStatus.harnesses),
// passed in rather than fetched again — setup-screen.test.tsx pins that walking
// the funnel makes exactly ONE getSetupStatus call.
function renderCard(operator = true, harnesses?: SetupHarnessTool[]) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={operator}>
        <ProvidersCard harnesses={harnesses} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const agent = (id: string, enabled = true): SetupHarnessTool => ({
  id,
  display: id,
  has_gateway: true,
  has_login: true,
  enabled,
  // a STORED row: the server stamps `mechanism` only when an agent row exists
  mechanism: "anthropic_api_key",
});

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  navigateMock.mockReset();
});

describe("ProvidersCard", () => {
  it("counts ENABLED rows, never hosts, and its link goes to /providers", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme"), row("acme-labs")]));
    renderCard();

    expect(await screen.findByText(PROVIDERS.CARD_PROVIDERS(2))).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.CARD_LEAD)).toBeInTheDocument();

    await userEvent.click(screen.getByText(PROVIDERS.CARD_OPEN));
    expect(navigateMock).toHaveBeenCalledWith("/providers");
  });

  it("a disabled row does not count toward the summary", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme"), row("off", true)]));
    renderCard();
    expect(await screen.findByText(PROVIDERS.CARD_PROVIDERS(1))).toBeInTheDocument();
  });

  it("no providers is its own sentence, and the link still goes where the teal Save providers lives", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([]));
    renderCard();
    expect(await screen.findByText(PROVIDERS.CARD_EMPTY)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.CARD_OPEN)).toBeInTheDocument();
  });

  it("a failed read leaves the summary absent rather than claiming there are no providers", async () => {
    getWorkspaceProvidersMock.mockRejectedValue(new Error("boom"));
    renderCard();
    expect(await screen.findByText(PROVIDERS.CARD_OPEN)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.CARD_EMPTY)).not.toBeInTheDocument();
  });

  // V1 lens E finding 6: the card is the funnel's `providers` step BODY and that
  // step is walked by everyone, so `return null` left a non-operator on a step
  // with a heading, an Optional badge and NOTHING underneath. The tier refusal
  // is said, the way /providers says it — and still no fetch, no link, no teal.
  it("says the tier to a caller without the SUPER tier, never an empty body, and asks the server nothing", () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
    renderCard(false);
    expect(screen.getByText(/requires the admin role/i)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.TITLE)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.CARD_OPEN)).not.toBeInTheDocument();
    expect(getWorkspaceProvidersMock).not.toHaveBeenCalled();
  });


  // V1 r2 MEDIUM: CARD_AGENTS and CARD_SUMMARY were frozen in the 98-key pin and
  // rendered NOWHERE — the card still summarised git rows alone, behind its own
  // header note saying the agent count "rides along once C-UI lands (W4)". W4
  // landed in the same delta.
  describe("the summary names both halves once the roster is known", () => {
    it("n git providers · m agents, from the roster's own `enabled`", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme"), row("acme-labs")]));
      renderCard(true, [agent("claude-code"), agent("codex-cli"), agent("none", false)]);
      expect(
        await screen.findByText(PROVIDERS.CARD_SUMMARY(PROVIDERS.CARD_PROVIDERS(2), PROVIDERS.CARD_AGENTS(2))),
      ).toBeInTheDocument();
    });

    // Legacy open mode: no agent row is stored, so every catalog row comes back
    // with `enabled` absent. Nothing has been ENABLED by an admin — the summary
    // is about the policy, not the catalog — so the agents half is omitted and
    // the card reads git providers alone (and CARD_EMPTY when those are zero
    // too: the providers e2e pins that on the funnel card). "3 agents" here
    // would claim a decision nobody made.
    it("a roster with no stored rows (legacy open mode: enabled on every row, no mechanism) names git providers alone", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
      renderCard(true, [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true }]);
      expect(await screen.findByText(PROVIDERS.CARD_PROVIDERS(1))).toBeInTheDocument();
      expect(screen.queryByText(PROVIDERS.CARD_AGENTS(1), { exact: false })).not.toBeInTheDocument();
    });

    it("a legacy roster with zero git rows is CARD_EMPTY", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([]));
      renderCard(true, [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true }]);
      expect(await screen.findByText(PROVIDERS.CARD_EMPTY)).toBeInTheDocument();
    });

    // Within a STAMPED roster a single row whose `enabled` is absent is UNKNOWN,
    // never false (setup.ts's rule) — it counts as offered.
    it("inside a roster with a policy, a row whose `enabled` is absent counts as offered", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
      renderCard(true, [agent("codex-cli", false), { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true }]);
      expect(
        await screen.findByText(PROVIDERS.CARD_SUMMARY(PROVIDERS.CARD_PROVIDERS(1), PROVIDERS.CARD_AGENTS(1))),
      ).toBeInTheDocument();
    });

    it("zero on BOTH sides is still CARD_EMPTY — one sentence, not '0 git providers · 0 agents'", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([]));
      renderCard(true, [agent("claude-code", false)]);
      expect(await screen.findByText(PROVIDERS.CARD_EMPTY)).toBeInTheDocument();
    });

    it("zero git rows with agents offered says so, rather than claiming nothing is enabled", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([]));
      renderCard(true, [agent("claude-code")]);
      expect(
        await screen.findByText(PROVIDERS.CARD_SUMMARY(PROVIDERS.CARD_PROVIDERS(0), PROVIDERS.CARD_AGENTS(1))),
      ).toBeInTheDocument();
      expect(screen.queryByText(PROVIDERS.CARD_EMPTY)).not.toBeInTheDocument();
    });

    it("an UNKNOWN roster (an older daemon, or status not landed) names git providers alone", async () => {
      getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
      renderCard(true, undefined);
      expect(await screen.findByText(PROVIDERS.CARD_PROVIDERS(1))).toBeInTheDocument();
    });

    it("a failed providers read leaves NO confident summary, roster or not", async () => {
      getWorkspaceProvidersMock.mockRejectedValue(new Error("boom"));
      renderCard(true, [agent("claude-code")]);
      expect(await screen.findByText(PROVIDERS.CARD_OPEN)).toBeInTheDocument();
      expect(screen.queryByText(PROVIDERS.CARD_EMPTY)).not.toBeInTheDocument();
      expect(screen.queryByText(PROVIDERS.CARD_AGENTS(1))).not.toBeInTheDocument();
    });
  });

  it("carries zero teal in either home", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
    renderCard();
    await screen.findByText(PROVIDERS.CARD_OPEN);
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal).toEqual([]);
  });
});
