/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps line the providers card raises for an admin (#1428, mock
// state 12a): a warning with its reason, never a blocker. The organisation
// check's findings are the row's own: the server keeps no last answer for this
// card to draw them from.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
  navigateMock.mockReset();
});

describe("ProvidersCard: the Azure DevOps line", () => {
  it("12a: a row the upgrade switched off says so, and Choose goes to the providers page", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([converted]));
    renderCard();
    const checks = await screen.findByTestId("ado-pat-checks");
    expect(within(checks).getByText(ADO_PAT.CONVERTED_CHECKLIST)).toBeInTheDocument();
    await userEvent.click(within(checks).getByRole("button", { name: ADO_PAT.CONVERTED_CHOOSE }));
    expect(navigateMock).toHaveBeenCalledWith("/admin/providers");
  });

  it("says nothing when no row needs a choice", async () => {
    getWorkspaceProvidersMock.mockResolvedValue(snap([ado({})]));
    renderCard();
    await screen.findByText(/git provider/);
    expect(screen.queryByTestId("ado-pat-checks")).not.toBeInTheDocument();
  });
});
