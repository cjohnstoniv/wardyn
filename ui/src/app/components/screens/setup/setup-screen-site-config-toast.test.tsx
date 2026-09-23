/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F6-F6 (Appendix A V8): SetupScreen's saveSiteConfig used to discard
// putSiteConfig's four advisory signals outright — an admin saving a proxy
// config naming a missing secret was told it saved cleanly. Kept out of
// setup-screen.test.tsx, which sits at the 1000-line file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection, SetupStatus } from "../../../lib/types";

const toastWarning = vi.fn();
vi.mock("sonner", () => ({
  toast: { warning: (...a: unknown[]) => toastWarning(...a), success: vi.fn(), error: vi.fn() },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a), completeOnboarding: vi.fn() },
}));
const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a), setSecret: vi.fn() },
}));
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const testProxyMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: () => Promise.resolve({ confinement_classes: ["CC1", "CC2"] }),
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: vi.fn(),
  },
}));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a), createWorkspace: vi.fn() },
}));
vi.mock("../../../lib/api/policies", () => ({ policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() } }));
const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({ drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) } }));
const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { createRun: vi.fn(), getRun: vi.fn(), killRun: vi.fn() } }));
vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../wardyn/live-approvals", () => ({ LiveApprovals: () => null }));

import { SetupScreen } from "./setup-screen";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";

function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    checks: [{ id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" }],
    ...overrides,
  });
}

function renderScreen() {
  return render(
    <MemoryRouter initialEntries={["/setup"]}>
      <SetupScreen onDone={() => {}} />
    </MemoryRouter>,
  );
}

const DETECTION: HostProxyDetection = {
  has_credentials: false,
  http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
};

beforeEach(() => {
  toastWarning.mockClear();
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus({ host_proxy: DETECTION }));
  listSecretsMock.mockReset().mockResolvedValue([]);
  getSiteConfigMock.mockReset().mockResolvedValue({});
  putSiteConfigMock.mockReset();
  testProxyMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "nothing to launch a probe with" });
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  getDrivesMock.mockReset().mockResolvedValue({ drives: [], grants: [], host_roots_configured: false, runner_target: "docker" });
  getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
});

async function saveDetectedProxy(user: ReturnType<typeof userEvent.setup>) {
  renderScreen();
  await screen.findByText("Fence");
  await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
  await screen.findAllByText("Single-user");
  await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
  await screen.findByRole("heading", { name: /^network$/i });
  await user.click(screen.getByRole("button", { name: /use detected proxy/i }));
}

describe("SetupScreen — the Corporate-network save toast", () => {
  // ticket: F6-F6
  it("warns on dangling_secret_refs, as an ADDITIONAL toast beside the step's own success toast", async () => {
    putSiteConfigMock.mockResolvedValue({
      siteConfig: { upstream_proxy_url: "http://proxy.corp.acme.com:8080" },
      danglingSecretRefs: ["upstream-proxy-secret"],
      onboardingCompletedAtIgnored: false,
      appliesFrom: "next_dispatch",
      sourcesNoLongerAdmitted: null,
    });
    const user = userEvent.setup();
    await saveDetectedProxy(user);

    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    await waitFor(() => expect(toastWarning).toHaveBeenCalledWith(expect.stringContaining("upstream-proxy-secret")));
  });

  // Negative control: a clean save (no dangling refs) never fires the F6-F6
  // warning — the step's own plain success toast (a different module, never
  // mocked here) is untouched by this fix.
  it("a clean save never fires the dangling-refs warning", async () => {
    putSiteConfigMock.mockResolvedValue({
      siteConfig: { upstream_proxy_url: "http://proxy.corp.acme.com:8080" },
      danglingSecretRefs: [],
      onboardingCompletedAtIgnored: false,
      appliesFrom: "next_dispatch",
      sourcesNoLongerAdmitted: null,
    });
    const user = userEvent.setup();
    await saveDetectedProxy(user);

    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });
});
