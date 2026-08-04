/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke-level only: each step body renders with minimal fixtures and shows one
// signature element; the deep orchestrator assertions live in
// setup-screen.test.tsx. Mocking conventions mirror setup-screen.test.tsx (same
// api-module mock shape, same baseStatus()). HostProxyStep/ArtifactRepoStep
// USED to be smoke-tested here; both bodies were deleted with the Corporate-
// network consolidation (their last caller was the Integrations "Add
// integration" dialog's mirror/proxy hand-off, and that hand-off retired), so
// their tests went too — the surviving behaviour is covered by
// setup/corp-network-step.test.tsx.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";

const getSetupStatusMock = vi.fn();
const listSecretsMock = vi.fn();
const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
const healthMock = vi.fn();
const listComposerBackendsMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const scanWorkspaceMock = vi.fn();

vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: (...a: unknown[]) => healthMock(...a),
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));
vi.mock("../../../lib/api/compose", () => ({
  composer: {
    listComposerBackends: (...a: unknown[]) => listComposerBackendsMock(...a),
  },
}));
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
  },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: { createRun: vi.fn() },
}));
// The wizard WorkspacesStep now mounts (Sources -> Base image -> Requirements
// -> Done) fetches this on mount too — swallows its own rejection either way
// (see wizard.tsx), mocked here for parity with wizard.test.tsx's own convention.
const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});

import { WorkspacesStep, ReviewStep, LaunchStep } from "./step-bodies";
import { deriveReadiness } from "../onboarding/intro";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";

// This suite's own pin is its `checks` array (gvisor/loopback/kvm/platform_wsl).
function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    checks: [
      { id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" },
      { id: "loopback", label: "Loopback bind", status: "warn", detail: "bound to 0.0.0.0" },
      { id: "kvm", label: "/dev/kvm", status: "fail", detail: "missing", fix: "enable virtualization" },
      {
        id: "platform_wsl",
        label: "WSL networking",
        status: "info",
        platform: "wsl",
        detail: "Running under WSL2",
      },
    ],
    ...overrides,
  });
}

describe("step-bodies.tsx — smoke", () => {
  beforeEach(() => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2"] });
    listComposerBackendsMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    scanWorkspaceMock.mockReset().mockResolvedValue({ async: false });
  });

  it("WorkspacesStep renders the empty-state onboard affordance", () => {
    // WorkspacesStep now navigates to a workspace's detail route (the wizard's
    // onOpenWorkspace and a non-ready row's Open button both call useNavigate),
    // so it needs a Router in scope even for this render-smoke assertion.
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText("No workspaces onboarded yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add workspace|onboard your first workspace/i })).toBeInTheDocument();
  });

  it("WorkspacesStep's 'Onboard your first workspace' opens the new four-step wizard (origin=setup)", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    await user.click(screen.getByRole("button", { name: /onboard your first workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
    expect(screen.getAllByText("Sources").length).toBeGreaterThan(0);
    // origin="setup": Done's primary action is "Back to setup", not "Open …→".
    // (Not reached by this smoke test — the step rail assertion above is the
    // load-bearing check that THIS wizard, not the retired panel, is mounted.)
  });

  it("ReviewStep renders the 'About this host' rollup", () => {
    const status = baseStatus();
    render(
      <ReviewStep
        status={status}
        readiness={deriveReadiness(status)}
        onRecheck={vi.fn()}
        rechecking={false}
        lastCheckedAt={null}
        onJump={vi.fn()}
      />,
    );
    expect(screen.getByText("About this host")).toBeInTheDocument();
  });

  it("LaunchStep renders the launch button", () => {
    render(<LaunchStep status={baseStatus()} onLaunch={vi.fn()} onOpenRuns={vi.fn()} canLaunch />);
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeInTheDocument();
  });

  // The inline launch button gates on a barrier only (canLaunch); with a barrier but
  // no model it launches with a non-blocking "no model connected" notice.
  it("LaunchStep gates on a barrier, then nudges (non-blocking) when no model is connected", () => {
    // No barrier → disabled + the barrier-required helper.
    const { rerender } = render(
      <LaunchStep status={baseStatus()} onLaunch={vi.fn()} onOpenRuns={vi.fn()} canLaunch={false} />,
    );
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeDisabled();
    expect(screen.getByText(/a sandbox barrier is required first/i)).toBeInTheDocument();
    expect(screen.queryByText(/no model connected/i)).not.toBeInTheDocument();

    // Barrier up, no model → ENABLED + the amber "no model connected" notice.
    rerender(
      <LaunchStep status={baseStatus()} onLaunch={vi.fn()} onOpenRuns={vi.fn()} canLaunch llmReady={false} />,
    );
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeEnabled();
    expect(screen.getByText(/no model connected/i)).toBeInTheDocument();

    // Barrier up + model connected → ENABLED, no notice.
    rerender(
      <LaunchStep status={baseStatus()} onLaunch={vi.fn()} onOpenRuns={vi.fn()} canLaunch llmReady />,
    );
    expect(screen.getByRole("button", { name: /launch your first run/i })).toBeEnabled();
    expect(screen.queryByText(/no model connected/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/a sandbox barrier is required first/i)).not.toBeInTheDocument();
  });
});
