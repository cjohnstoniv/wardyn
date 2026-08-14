/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Detail — base-component model (B3): Secrets (w/ delivery language), Egress,
// Config, Verification (w/ re-test), Used-by, delete-409 note. No capability
// table — that was the legacy screen's own richness; a generic kind is
// base-only, and the closed kinds differ only in prefill.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import type { WireIntegration } from "../../../lib/types/setup";

vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: { harnessDisconnect: vi.fn(), harnessLogin: vi.fn(), harnessCredentialPaste: vi.fn() },
}));
vi.mock("../../../lib/api/health", () => ({ health: { getSiteConfig: vi.fn(), putSiteConfig: vi.fn() } }));

const listMock = vi.fn();
const adoptIntegrationMock = vi.fn();
const putIntegrationMock = vi.fn();
const removeIntegrationMock = vi.fn();
const testIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return {
    ...actual,
    integrationsApi: { ...actual.integrationsApi, adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a) },
    genericIntegrationsApi: {
      ...actual.genericIntegrationsApi,
      list: (...a: unknown[]) => listMock(...a),
      put: (...a: unknown[]) => putIntegrationMock(...a),
      remove: (...a: unknown[]) => removeIntegrationMock(...a),
      test: (...a: unknown[]) => testIntegrationMock(...a),
    },
  };
});

import { IntegrationDetailScreen } from "./integration-detail";

function renderDetail(id: string, operator = true) {
  return render(
    <MemoryRouter initialEntries={[`/integrations/${encodeURIComponent(id)}`]}>
      <OperatorProvider operator={operator}>
        <Routes>
          <Route path="/integrations/:id" element={<IntegrationDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const FEED_ROW: WireIntegration = {
  id: "corp-artifactory",
  name: "Corp Artifactory",
  kind: "artifactory",
  source: "stored",
  egress: ["artifactory.corp.internal"],
  secrets: [{ role: "api_key", secret_name: "artifactory-token", delivery: { mode: "proxy_header", header: "Authorization", format: "Bearer %s" } }],
  probe: { method: "GET", url: "https://artifactory.corp.internal/" },
  probe_status: { state: "not_tested" },
};

const BEDROCK_ROW: WireIntegration = {
  id: "bedrock",
  name: "AWS Bedrock",
  kind: "bedrock",
  source: "legacy",
  config: { auth_lane: "bearer", region: "us-east-1", model: "anthropic.claude-3" },
  secrets: [{ role: "api_key", secret_name: "bedrock-bearer-token", delivery: { mode: "proxy_header" } }],
};

describe("IntegrationDetailScreen — base sections", () => {
  it("renders secrets w/ delivery language, egress, and the verification chip", async () => {
    listMock.mockResolvedValue([FEED_ROW]);
    renderDetail("corp-artifactory");

    await screen.findByText("Corp Artifactory");
    expect(screen.getByText("artifactory-token")).toBeInTheDocument();
    expect(screen.getByText(/Never resident/)).toBeInTheDocument();
    expect(screen.getByText("artifactory.corp.internal")).toBeInTheDocument();
    expect(screen.getByText("not tested")).toBeInTheDocument();
  });

  it("renders a bedrock row's config keys and resident secret language", async () => {
    listMock.mockResolvedValue([BEDROCK_ROW]);
    renderDetail("bedrock");

    await screen.findByText("AWS Bedrock");
    expect(screen.getByText("region")).toBeInTheDocument();
    expect(screen.getByText("us-east-1")).toBeInTheDocument();
    expect(screen.getByText("bedrock-bearer-token")).toBeInTheDocument();
  });

  it("a derived row shows Adopt to edit instead of the stored chip", async () => {
    listMock.mockResolvedValue([BEDROCK_ROW]);
    renderDetail("bedrock");

    await screen.findByText("AWS Bedrock");
    expect(screen.getByRole("button", { name: "Adopt to edit" })).toBeInTheDocument();
    expect(screen.queryByText("stored")).not.toBeInTheDocument();
  });

  it("re-testing calls test() and renders the verdict its response body carries, never inferring pass from a 2xx", async () => {
    const user = userEvent.setup();
    listMock.mockResolvedValue([FEED_ROW]);
    testIntegrationMock.mockResolvedValue({ state: "passed", checked_at: "2026-08-14T00:00:00Z" });
    renderDetail("corp-artifactory");

    await screen.findByText("Corp Artifactory");
    await user.click(screen.getByRole("button", { name: /re-test/i }));

    await waitFor(() => expect(testIntegrationMock).toHaveBeenCalledWith("corp-artifactory"));
    await screen.findByText("verified");
  });

  it("a row with no probe configured offers no Test button", async () => {
    listMock.mockResolvedValue([{ ...FEED_ROW, probe: undefined }]);
    renderDetail("corp-artifactory");

    await screen.findByText("Corp Artifactory");
    expect(screen.queryByRole("button", { name: /re-test/i })).not.toBeInTheDocument();
    expect(screen.getByText(/no probe configured/i)).toBeInTheDocument();
  });

  it("Used-by states the real default-for marks, honestly, with no fabricated workspace count", async () => {
    listMock.mockResolvedValue([{ ...FEED_ROW, kind: "anthropic_api_key", source: "stored", default_for: ["agent_runs"] }]);
    renderDetail("corp-artifactory");

    await screen.findByText("Corp Artifactory");
    expect(screen.getByText("Default for agent runs.")).toBeInTheDocument();
    expect(screen.getByText(/binding isn't surfaced here yet/i)).toBeInTheDocument();
  });

  it("names the delete-409 possibility in the danger zone", async () => {
    listMock.mockResolvedValue([FEED_ROW]);
    renderDetail("corp-artifactory");

    await screen.findByText("Corp Artifactory");
    expect(screen.getByText(/A 409 here means something still depends/i)).toBeInTheDocument();
  });

  it("an unknown id renders the not-found state instead of crashing", async () => {
    listMock.mockResolvedValue([FEED_ROW]);
    renderDetail("does-not-exist");

    expect(await screen.findByText("Integration not found")).toBeInTheDocument();
  });
});
