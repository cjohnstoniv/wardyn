/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { baseStatus } from "../setup/test-fixtures";
import { T } from "../../../lib/integrations";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));

const listSecretsMock = vi.fn();
const deleteSecretMock = vi.fn();
const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));

const harnessDisconnectMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessDisconnect: (...a: unknown[]) => harnessDisconnectMock(...a),
    harnessLogin: vi.fn(),
    harnessCredentialPaste: vi.fn(),
  },
}));

import { IntegrationDetailScreen } from "./integration-detail";

function renderDetail(id: string) {
  return render(
    <MemoryRouter initialEntries={[`/integrations/${id}`]}>
      <Routes>
        <Route path="/integrations/:id" element={<IntegrationDetailScreen />} />
        <Route path="/integrations" element={<div>back on the list</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("IntegrationDetailScreen — blast-radius confirm", () => {
  beforeEach(() => {
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
  });

  it("Delete integration lists the computed blast radius, then really deletes the backing secret", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    renderDetail("ai:anthropic_api_key");
    await screen.findByRole("heading", { name: "Anthropic (API key)" });

    // The danger-zone trigger (not the header kebab's menuitem of the same name).
    await user.click(screen.getByRole("button", { name: /delete integration…/i }));

    // The SAME computed lines also sit inline in the (always-visible) Danger
    // zone region, so scope to the confirm dialog itself to avoid double-matches.
    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(/first model call fails/i)).toBeInTheDocument();
    expect(dialog.getByText(/anthropic-api-key.*not deleted.*Secrets/i)).toBeInTheDocument();

    await user.click(dialog.getByRole("button", { name: /^delete integration$/i }));
    await waitFor(() => expect(deleteSecretMock).toHaveBeenCalledWith("anthropic-api-key"));
    // A successful delete returns to the list.
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });

  it("a harness-backed subscription's blast radius says disconnecting is the removal, and Delete calls harnessDisconnect", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ harness: [{ provider: "anthropic", captured: true, captured_at: new Date().toISOString() }] }),
    );
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    harnessDisconnectMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    renderDetail("ai:anthropic_subscription:managed");
    await screen.findByRole("heading", { name: "Claude subscription (managed)" });

    await user.click(screen.getByRole("button", { name: /delete integration…/i }));
    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(/disconnecting IS the removal/i)).toBeInTheDocument();

    await user.click(dialog.getByRole("button", { name: /^delete integration$/i }));
    await waitFor(() => expect(harnessDisconnectMock).toHaveBeenCalledWith("anthropic"));
  });

  it("shows a not-found panel with a way back for a stale/unknown id", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);

    renderDetail("ai:does_not_exist");
    expect(await screen.findByText(/integration not found/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /back to integrations/i })).toBeInTheDocument();
  });

  // A bookmarked mirror:/proxy: link from before the Corporate-network
  // consolidation. Those rows aren't derived any more, so the honest answer is
  // not-found — never a resurrected detail page for a category the list no
  // longer has, and never a danger zone offering to delete site config.
  it("a pre-consolidation mirror/proxy id is not found, even with that config still present", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/x" }],
      upstream_proxy_url: "http://proxy.corp.acme.com:8080",
    });
    listSecretsMock.mockResolvedValue([]);

    const { unmount } = renderDetail("mirror:0");
    expect(await screen.findByText(/integration not found/i)).toBeInTheDocument();
    unmount();

    renderDetail("proxy:host");
    expect(await screen.findByText(/integration not found/i)).toBeInTheDocument();
  });

  it("renders the GitHub App's ruleset row Unknown (never fabricated) with T.CACHE_CAVEAT", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: [], github_app: true } }));
    getSiteConfigMock.mockResolvedValue({ scm_hosts: ["github.com"] });
    listSecretsMock.mockResolvedValue([]);

    renderDetail("scm:github.com");
    await screen.findByRole("heading", { name: "GitHub" });

    expect(screen.getByText(/Unknown/)).toBeInTheDocument();
    expect(screen.getByText(T.CACHE_CAVEAT)).toBeInTheDocument();
  });
});
