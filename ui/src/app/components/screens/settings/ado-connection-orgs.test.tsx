/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { expandCard } from "../../../lib/test-dom";

const getMineMock = vi.fn();
vi.mock("../../../lib/api/scm-access", () => ({
  scmAccess: { getMine: (...a: unknown[]) => getMineMock(...a) },
}));
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: vi.fn(), connectFallback: vi.fn(), blockedUrl: null }),
}));

import { AdoConnectionCard } from "./ado-connection";

const own = { token_mode: "own_pat", source: "own", max_days: 30, token_scopes: ["Code (Read)"] };
const acme = { ...own, state: "live", org: "https://dev.azure.com/acme", expires_on: "2000-11-27" };
const beta = { ...own, state: "not_configured", org: "https://dev.azure.com/beta" };

function status(scm_access: object): SetupStatus {
  return { ready: true, checks: [], auth: { mode: "local" }, runner: { driver: "docker", confinement_classes: [] }, providers: [], secrets: { present: [] }, age_key: { durable: true }, has_runs: false, scm_access } as unknown as SetupStatus;
}

const renderCard = () => render(<MemoryRouter initialEntries={["/account"]}><AdoConnectionCard status={status(acme)} onChanged={vi.fn()} /></MemoryRouter>);

// #2146: Settings > Azure DevOps shows one own-token section per organisation.
describe("AdoConnectionCard with several own-token organisations", () => {
  beforeEach(() => getMineMock.mockReset());

  it("a section each: acme holds a token, beta has none and offers Add", async () => {
    getMineMock.mockResolvedValue([acme, beta]);
    renderCard();
    await expandCard(PROVIDERS.KIND_AZURE_DEVOPS);
    const betaSection = await screen.findByTestId("ado-org-beta");
    expect(within(betaSection).getByRole("button", { name: ADO_PAT.OWN_ADD_CTA })).toBeInTheDocument();
    const acmeSection = screen.getByTestId("ado-org-acme");
    expect(within(acmeSection).getByRole("button", { name: ADO_PAT.OWN_REPLACE })).toBeInTheDocument();
    expect(within(acmeSection).queryByRole("button", { name: ADO_PAT.OWN_ADD_CTA })).not.toBeInTheDocument();
  });

  it("one organisation: the card is the single one it was", async () => {
    getMineMock.mockResolvedValue([acme]);
    renderCard();
    await expandCard(PROVIDERS.KIND_AZURE_DEVOPS);
    expect(screen.queryByTestId("ado-own-token-cards")).not.toBeInTheDocument();
    expect(screen.getByTestId("ado-pat-card")).toBeInTheDocument();
  });
});
