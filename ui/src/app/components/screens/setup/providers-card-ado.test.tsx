/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps lines the providers card raises for an admin (#1428, mock
// state 12a and the setup-checklist half of states 3 and 8c): each a warning
// with its reason, never a blocker.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/providers")>("../../../lib/api/providers");
  return { ...actual, providers: { ...actual.providers, getWorkspaceProviders: () => getWorkspaceProvidersMock() } };
});

const healthMock = vi.fn();
vi.mock("../../../lib/api/ado-pat", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/ado-pat")>("../../../lib/api/ado-pat");
  return { ...actual, adoPat: { ...actual.adoPat, health: () => healthMock() } };
});

const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});

import type { GitProvider } from "../../../lib/api/providers";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ProvidersCard } from "./providers-card";

const snap = (rows: GitProvider[]) => ({ providers: { git: rows }, etag: '"abc"' });
const ado = (over: Partial<GitProvider>): GitProvider => ({
  id: "ado",
  kind: "azure_devops",
  base_urls: ["https://dev.azure.com/wardyn-live-test"],
  lanes: ["entra"],
  credential_source: "per_user",
  entra: { tenant_id: "t", client_id: "c", token_mode: "minted_pat" },
  ...over,
});
const converted = ado({ disabled: true, entra: { tenant_id: "", client_id: "", token_mode: "own_pat" } });

function renderCard() {
  render(
    <MemoryRouter>
      <OperatorProvider operator>
        <ProvidersCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  healthMock.mockReset();
  healthMock.mockResolvedValue({});
  navigateMock.mockReset();
});

describe("ProvidersCard: Azure DevOps lines", () => {
  it("12a: a row the upgrade switched off says so, and Choose goes to the providers page", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([converted]));
    renderCard();
    const checks = await screen.findByTestId("ado-pat-checks");
    expect(within(checks).getByText(ADO_PAT.CONVERTED_CHECKLIST)).toBeInTheDocument();
    await userEvent.click(within(checks).getByRole("button", { name: ADO_PAT.CONVERTED_CHOOSE }));
    expect(navigateMock).toHaveBeenCalledWith("/admin/providers");
  });

  it("says nothing when no row needs a choice and the organisation has raised nothing", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([ado({})]));
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("ado-pat-checks")).not.toBeInTheDocument();
  });

  it("3: a lifespan the organisation refused is a line with the longest life it accepted", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([ado({})]));
    healthMock.mockResolvedValue({ lifespan: "too_long", lifespan_hours: 24 });
    renderCard();
    expect(await screen.findByText(ADO_PAT.LIFESPAN_REFUSAL(24))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO_PAT.CONVERTED_CHOOSE })).not.toBeInTheDocument();
  });

  it("8c: an organisation that blocks token creation names the person", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([ado({})]));
    healthMock.mockResolvedValue({ blocked_person: "Priya Shah" });
    renderCard();
    expect(await screen.findByText(ADO_PAT.POLICY_BANNER("Priya Shah"))).toBeInTheDocument();
  });

  it("a deployment with no row that creates tokens never asks for the check", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([ado({ entra: { tenant_id: "t", client_id: "c" } })]));
    renderCard();
    await screen.findByText(/1 git provider|git provider/);
    expect(healthMock).not.toHaveBeenCalled();
  });
});
