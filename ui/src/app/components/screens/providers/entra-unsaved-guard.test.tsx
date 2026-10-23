/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Entra settings are part of the Git draft providers-screen.tsx guards, so editing one is an unsaved change.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "../../../lib/test-fixtures";
import { UnsavedGuardProvider } from "../../../lib/use-unsaved-guard";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import { ADO_ENTRA_EDITOR as E } from "../../../lib/workspace-providers-copy";
import type { GitProvider } from "../../../lib/api/providers";
import { setField } from "../../../../test/set-field";
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
    <MemoryRouter>
      <UnsavedGuardProvider>
        <OperatorProvider operator={operator}>
          <ProvidersScreen />
        </OperatorProvider>
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
}


const TENANT = "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f";
const adoRow = (): GitProvider => ({
  id: "azure_devops",
  kind: "azure_devops",
  base_urls: ["https://dev.azure.com/wardyn-live-test"],
  lanes: ["entra"],
  credential_source: "per_user",
  entra: { tenant_id: TENANT, client_id: "3b241101-e2bb-4255-8caf-4136c566a962", capability_ceiling: ["project_read", "code_read"], default_profile: [], token_mode: "minted_pat" },
});

afterEach(() => cleanup());

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  getWorkspaceProvidersMock.mockResolvedValue({ providers: { git: [adoRow()] }, etag: '"e1"' });
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getAgentProvidersMock.mockReset();
  getAgentProvidersMock.mockResolvedValue({ providers: {}, etag: '"a0"' });
});

describe("ProvidersScreen: Entra settings unsaved guard", () => {
  it("editing the tenant id registers an unsaved change, and typing it back clears it", async () => {
    renderScreen();
    const tenant = await screen.findByLabelText(E.FIELD_TENANT);
    expect(unsavedSnapshot()).toBeNull();
    setField(tenant, "00000000-0000-4000-8000-000000000000");
    expect(unsavedSnapshot()).not.toBeNull();
    setField(tenant, TENANT);
    expect(unsavedSnapshot()).toBeNull();
  });

  it("leaves nothing registered once the screen unmounts", async () => {
    const { unmount } = renderScreen();
    setField(await screen.findByLabelText(E.FIELD_TENANT), "00000000-0000-4000-8000-000000000000");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
