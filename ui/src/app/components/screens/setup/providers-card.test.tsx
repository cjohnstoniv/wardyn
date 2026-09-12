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

function renderCard(operator = true) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={operator}>
        <ProvidersCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

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

  it("renders nothing for a caller without the SUPER tier, and asks the server nothing", () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
    renderCard(false);
    expect(screen.queryByTestId("providers-card")).not.toBeInTheDocument();
    expect(getWorkspaceProvidersMock).not.toHaveBeenCalled();
  });

  it("carries zero teal in either home", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([row("acme")]));
    renderCard();
    await screen.findByText(PROVIDERS.CARD_OPEN);
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal).toEqual([]);
  });
});
