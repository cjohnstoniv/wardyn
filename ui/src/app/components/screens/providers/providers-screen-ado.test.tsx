/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /providers screen with an Azure DevOps row that creates tokens or takes a
// pasted one (#1428): the row the upgrade switched off saves and turns on in one
// step, and a refused Save says why under the choice it is about.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "../../../lib/test-fixtures";
import { UnsavedGuardProvider } from "../../../lib/use-unsaved-guard";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import type { GitProvider } from "../../../lib/api/providers";
import { ProvidersScreen } from "./providers-screen";

const getWorkspaceProvidersMock = vi.fn();
const putWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/providers")>("../../../lib/api/providers");
  return {
    ...actual,
    providers: {
      ...actual.providers,
      getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a),
      putWorkspaceProviders: (...a: unknown[]) => putWorkspaceProvidersMock(...a),
    },
  };
});

const getAgentProvidersMock = vi.fn();
const putAgentProvidersMock = vi.fn();
vi.mock("../../../lib/api/agent-providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/agent-providers")>("../../../lib/api/agent-providers");
  return {
    ...actual,
    agentProviders: {
      ...actual.agentProviders,
      getAgentProviders: (...a: unknown[]) => getAgentProvidersMock(...a),
      putAgentProviders: (...a: unknown[]) => putAgentProvidersMock(...a),
    },
  };
});

// The Agents tab also reads GET /model-providers (its rows' defaults).
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: {
    getModelProviders: () => Promise.resolve({ providers: {}, connected: {}, etag: null }),
  },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => Promise.resolve({ drives: [], grants: [], host_roots_configured: false, runner_target: "" }) } };
});

vi.mock("../../../lib/api/base-images", () => ({
  baseImages: { list: () => Promise.resolve([]), add: vi.fn() },
}));

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => vi.fn() };
});

// #460 review — wrapped in UnsavedGuardProvider even for tests that never
// dirty anything: it's a no-op while clean (the noopGuard-shaped default),
// so every existing assertion below is unaffected, and it's what the tab-
// switch-guard tests need to see the real confirm dialog rather than the
// context's own no-provider fallback (which would proceed silently).
function renderScreen(operator = true) {
  return render(
    <UnsavedGuardProvider>
      <OperatorProvider operator={operator}>
        <ProvidersScreen />
      </OperatorProvider>
    </UnsavedGuardProvider>,
  );
}

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  putWorkspaceProvidersMock.mockReset();
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getAgentProvidersMock.mockReset();
  getAgentProvidersMock.mockResolvedValue({ providers: {}, etag: '"a0"' });
  putAgentProvidersMock.mockReset();
});


const adoRow = (over: Partial<GitProvider> = {}): GitProvider => ({
  id: "azure_devops",
  kind: "azure_devops",
  base_urls: ["https://dev.azure.com/wardyn-live-test"],
  lanes: ["entra"],
  credential_source: "per_user",
  entra: { tenant_id: "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f", client_id: "3b241101-e2bb-4255-8caf-4136c566a962", capability_ceiling: ["read"], default_profile: [], token_mode: "minted_pat" },
  ...over,
});

const converted = () =>
  adoRow({ disabled: true, entra: { tenant_id: "", client_id: "", capability_ceiling: ["read"], default_profile: [], token_mode: "own_pat" } });

describe("ProvidersScreen: Azure DevOps token choices", () => {
  it("12b: a converted row shows why it is off, and Save and turn on saves the document with the row on", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [converted()] }, etag: '"e1"' });
    putWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [{ ...converted(), disabled: false }] }, etag: '"e2"', sourcesNoLongerAdmitted: 0 });
    renderScreen();
    expect(await screen.findByText(ADO_PAT.CONVERTED_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.ROW_DISABLED_HINT)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.CONVERTED_SAVE_ON }));
    await waitFor(() => expect(putWorkspaceProvidersMock).toHaveBeenCalledTimes(1));
    const [sent, etag] = putWorkspaceProvidersMock.mock.calls[0] as [{ git: GitProvider[] }, string];
    expect(etag).toBe('"e1"');
    expect(sent.git).toHaveLength(1);
    expect(sent.git[0].disabled).toBe(false);
    expect(sent.git[0].entra?.token_mode).toBe("own_pat");
    // Saved and on: the note is gone, the row's own editor is up.
    await waitFor(() => expect(screen.queryByText(ADO_PAT.CONVERTED_NOTE)).not.toBeInTheDocument());
  });

  it("a row that is off by choice, not by the upgrade, keeps the plain off hint", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [adoRow({ disabled: true })] }, etag: '"e1"' });
    renderScreen();
    expect(await screen.findByText(PROVIDERS.ROW_DISABLED_HINT)).toBeInTheDocument();
    expect(screen.queryByText(ADO_PAT.CONVERTED_NOTE)).not.toBeInTheDocument();
  });

  it("3: a Save the server refused for want of a client secret says so under the token choice", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [adoRow()] }, etag: '"e1"' });
    putWorkspaceProvidersMock.mockRejectedValue(new HttpError(400, ADO_PAT.NO_CLIENT_SECRET));
    renderScreen();
    await screen.findByRole("radiogroup", { name: ADO_PAT.SECTION_TITLE });
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    const inline = await screen.findAllByText(ADO_PAT.NO_CLIENT_SECRET);
    // The screen's own refusal banner and the choice's inline line both carry it.
    expect(inline.length).toBe(2);
  });

  it("a lifetime the server would refuse withholds Save", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [adoRow({ entra: { ...adoRow().entra!, pat_max_hours: 400 } })] }, etag: '"e1"' });
    renderScreen();
    await screen.findByRole("radiogroup", { name: ADO_PAT.SECTION_TITLE });
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeDisabled();
  });
});
