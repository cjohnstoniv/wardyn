/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from setup-screen.test.tsx (which sits at the file-size gate's 1000-line
// cap — see scripts/check-file-size.sh) rather than grown past it. Covers one
// thing setup-screen.test.tsx's suite doesn't: the `operatorResolved && operator`
// guard on reloadSiteConfig/loadSecrets/loadProviderCount — the member's own
// Getting Started must never call an admin-only endpoint. Mirrors
// setup-screen.test.tsx's mock harness verbatim — this suite mounts the same
// orchestrator, just with an explicit <OperatorProvider> instead of the
// no-provider fail-open default.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";

const getSetupStatusMock = vi.fn();
const listSecretsMock = vi.fn();
const setSecretMock = vi.fn();
const healthMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const testProxyMock = vi.fn();
const testRedirectMock = vi.fn();

const completeOnboardingMock = vi.fn(async () => {});
vi.mock("../../../lib/api/setup", () => ({
  setup: {
    getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
    completeOnboarding: () => completeOnboardingMock(),
  },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: (...a: unknown[]) => healthMock(...a),
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: (...a: unknown[]) => testRedirectMock(...a),
  },
}));
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a), createWorkspace: vi.fn() },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() },
}));
const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({
  drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) },
}));
const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: { createRun: vi.fn(), getRun: vi.fn(), killRun: vi.fn() },
}));
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: () => null,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: () => null,
}));

import { SetupScreen } from "./setup-screen";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";

function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    checks: [
      { id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" },
      { id: "loopback", label: "Loopback bind", status: "warn", detail: "bound to 0.0.0.0" },
      { id: "kvm", label: "/dev/kvm", status: "fail", detail: "missing", fix: "enable virtualization" },
      { id: "macos-kvm", label: "macOS note", status: "info", detail: "CC3 unavailable on macOS" },
    ],
    ...overrides,
  });
}

function renderScreen(operator: boolean, initialStatus?: SetupStatus) {
  return render(
    <MemoryRouter initialEntries={["/setup"]}>
      <OperatorProvider operator={operator}>
        <SetupScreen onDone={() => {}} initialStatus={initialStatus} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("SetupScreen — operatorResolved && operator guard on admin-only reads", () => {
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2"] });
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    // putSiteConfig returns the four advisory signals, not void.
    putSiteConfigMock.mockReset().mockResolvedValue({
      siteConfig: {},
      danglingSecretRefs: [],
      onboardingCompletedAtIgnored: false,
      appliesFrom: "next_dispatch",
      sourcesNoLongerAdmitted: null,
    });
    getDrivesMock
      .mockReset()
      .mockResolvedValue({ drives: [], grants: [], host_roots_configured: false, runner_target: "docker" });
    getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
    testProxyMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "no runner configured, nothing to launch a probe with" });
    testRedirectMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "no runner configured, nothing to launch a probe with" });
  });

  it("a member's cold /setup load fires no admin-only reads", async () => {
    renderScreen(/* operator */ false);
    // Let the mount effect's whole fetch chain settle before asserting the
    // negative — a premature assert would pass vacuously.
    expect(
      await screen.findByRole("heading", { name: /pick your barrier/i }),
    ).toBeInTheDocument();
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(getSiteConfigMock).not.toHaveBeenCalled();
    expect(listSecretsMock).not.toHaveBeenCalled();
    expect(getWorkspaceProvidersMock).not.toHaveBeenCalled();
  });

  it("an admin's cold /setup load still fetches all three", async () => {
    renderScreen(/* operator */ true);
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    await waitFor(() => expect(getSiteConfigMock).toHaveBeenCalled());
    await waitFor(() => expect(listSecretsMock).toHaveBeenCalled());
    await waitFor(() => expect(getWorkspaceProvidersMock).toHaveBeenCalled());
  });
});

// #806: a gated install lands here straight from App.tsx's own /setup/status
// read. The funnel's rail must paint from that status, not wait on the
// screen's second read of the same endpoint — held forever here, so the only
// way the rail can appear is from the status handed in.
describe("SetupScreen — paints from the status it is handed (#806)", () => {
  it("renders the step rail while its own /setup/status read is still pending", async () => {
    getSetupStatusMock.mockReset().mockReturnValue(new Promise(() => {}));
    listSecretsMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
    renderScreen(/* operator */ true, baseStatus());
    expect((await screen.findAllByRole("navigation", { name: "Setup steps" })).length).toBeGreaterThan(0);
    expect(screen.getByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(getSetupStatusMock).toHaveBeenCalled();
  });
});
