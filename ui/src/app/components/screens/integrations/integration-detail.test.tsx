/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Detail — base-component model (B3): Secrets (w/ delivery language), Egress,
// Config, Verification (w/ re-test), Used-by, delete-409 note. No capability
// table — that was the legacy screen's own richness; a generic kind is
// base-only, and the closed kinds differ only in prefill.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { T } from "../../../lib/integrations";
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

// ui-integrations-5: the list screen shows a Viewer-role chip + explanatory
// line when !operator; the detail screen had no such signal at all — only
// per-button disablement (and per ui-integrations-3/6, some of those didn't
// even explain themselves).
describe("IntegrationDetailScreen — viewer role", () => {
  it("renders the Viewer-role chip and line, matching the list screen", async () => {
    listMock.mockResolvedValue([FEED_ROW]);
    renderDetail("corp-artifactory", false);

    await screen.findByText("Corp Artifactory");
    expect(screen.getByText("Viewer role")).toBeInTheDocument();
    expect(screen.getByText(T.VIEWER_LINE)).toBeInTheDocument();
  });

  it("an operator sees neither the chip nor the line", async () => {
    listMock.mockResolvedValue([FEED_ROW]);
    renderDetail("corp-artifactory", true);

    await screen.findByText("Corp Artifactory");
    expect(screen.queryByText("Viewer role")).not.toBeInTheDocument();
  });
});

// ui-integrations-3 + ui-integrations-6: disabled controls on this page used
// to fall back to native `title` (Adopt to edit, Danger-zone Delete) or say
// nothing at all (the Used-by Set/Unset default buttons) — every one of them
// now names the reason as visible button content, like the list row's kebab.
describe("IntegrationDetailScreen — viewer-disabled controls all explain themselves", () => {
  const AI_STORED_ROW: WireIntegration = { ...FEED_ROW, kind: "anthropic_api_key", source: "stored", default_for: ["agent_runs"] };

  it("the Used-by Set/Unset default buttons carry the operator-only reason when disabled", async () => {
    listMock.mockResolvedValue([AI_STORED_ROW]);
    renderDetail("corp-artifactory", false);

    await screen.findByText("Corp Artifactory");
    const unset = screen.getByRole("button", { name: /unset default for agent runs/i });
    expect(unset).toBeDisabled();
    expect(within(unset).getByText(/requires the operator role/i)).toBeInTheDocument();
  });

  it("'Adopt to edit' and the Danger-zone Delete button name the reason visibly, not via title", async () => {
    listMock.mockResolvedValue([{ ...BEDROCK_ROW, source: "legacy" as const }]);
    renderDetail("bedrock", false);

    await screen.findByText("AWS Bedrock");
    const adopt = screen.getByRole("button", { name: /adopt to edit/i });
    expect(adopt).toBeDisabled();
    expect(within(adopt).getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(adopt).not.toHaveAttribute("title");

    const del = screen.getByRole("button", { name: /delete integration…/i });
    expect(del).toBeDisabled();
    expect(within(del).getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(del).not.toHaveAttribute("title");
  });
});

// ui-integrations-4: wire.disabled ("Off") used to be read-only on this page
// too — no control ever wrote it. A stored row now offers Enable/Disable next
// to the "stored" chip, PUTting the flipped flag through toggleDisabled.
describe("IntegrationDetailScreen — enable/disable a stored row", () => {
  it("a stored, enabled row offers 'Disable' and PUTs disabled: true", async () => {
    const user = userEvent.setup();
    listMock.mockResolvedValue([FEED_ROW]); // stored, disabled undefined
    renderDetail("corp-artifactory");
    await screen.findByText("Corp Artifactory");

    await user.click(screen.getByRole("button", { name: "Disable" }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("corp-artifactory", expect.objectContaining({ disabled: true })),
    );
  });

  it("a stored, disabled row offers 'Enable' and PUTs disabled: false", async () => {
    const user = userEvent.setup();
    listMock.mockResolvedValue([{ ...FEED_ROW, disabled: true }]);
    renderDetail("corp-artifactory");
    await screen.findByText("Corp Artifactory");

    await user.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("corp-artifactory", expect.objectContaining({ disabled: false })),
    );
  });

  it("a derived (non-stored) row offers no Enable/Disable control — PUT would 409", async () => {
    listMock.mockResolvedValue([BEDROCK_ROW]); // source: "legacy"
    renderDetail("bedrock");
    await screen.findByText("AWS Bedrock");

    expect(screen.queryByRole("button", { name: /^(enable|disable)$/i })).not.toBeInTheDocument();
  });
});
