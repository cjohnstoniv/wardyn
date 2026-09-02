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
// setup/corp-network-step.test.tsx. SourcesStep/ImagesStep went the same way
// when sources-library.tsx/image-catalog.tsx were deleted.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";

const getSetupStatusMock = vi.fn();
const listSecretsMock = vi.fn();
const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
const healthMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const createWorkspaceMock = vi.fn();

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
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a),
  },
}));
// The Workspaces step's user-drives card summarises GET /drives.
const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({
  drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: { createRun: vi.fn() },
}));

import { ReviewStep, WorkspacesStep } from "./step-bodies";
import { deriveReadiness } from "../../../lib/readiness";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import type { Workspace } from "../../../lib/types";

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

function ws(overrides: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "demo-workspace",
    kind: "local_dir",
    source: "/home/dev/demo-workspace",
    status: "scanned",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("step-bodies.tsx — smoke", () => {
  beforeEach(() => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2"] });
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    createWorkspaceMock.mockReset();
    getDrivesMock
      .mockReset()
      .mockResolvedValue({ drives: [], grants: [], host_roots_configured: false, runner_target: "docker" });
  });

  // Persistent storage is not a workspace and gets no step of its own, so this
  // card under the list is the only place the funnel mentions it — the SAME
  // component Settings draws as its fifth card. SUPER-only, and the gate is the
  // provider's, not a guess: a security admin gets no card and the step asks
  // GET /drives nothing.
  it("WorkspacesStep carries the user-drives card, and only for the SUPER tier", async () => {
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(await screen.findByTestId("user-drives-card")).toBeInTheDocument();

    cleanup();
    getDrivesMock.mockClear();
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} securityOperator>
          <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("No workspaces onboarded yet.")).toBeInTheDocument();
    expect(screen.queryByTestId("user-drives-card")).toBeNull();
    expect(getDrivesMock).not.toHaveBeenCalled();
  });

  // sources-library.tsx/image-catalog.tsx are gone — SourcesStep/ImagesStep
  // went with them. "Your work" is just WorkspacesStep now.
  it("WorkspacesStep renders the empty-state onboard affordance, which opens the one Add workspace dialog", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText("No workspaces onboarded yet.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /onboard your first workspace/i }));
    // AddWorkspaceDialog — the same one /workspaces uses, never the retired wizard.
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Repository" })).toBeInTheDocument();
  });

  // UX-4: the row used to re-implement the single-source sub-line by hand
  // ({kind === "repo" ? "repo" : "local dir"} · {w.source}), so a multi-source
  // workspace — this range's own headline shape — rendered "local dir ·" with
  // a blank path. sourceSubLine (workspaces.tsx) is the one helper the detail
  // page's header already reuses for exactly this reason.
  it("a multi-source workspace's row renders the composition summary via sourceSubLine, not a hand-rolled single-source line", async () => {
    const multi = ws({
      id: "ws-multi",
      name: "multi-source workspace",
      sources: [
        { type: "local_dir", path: "/a" },
        { type: "local_dir", path: "/b" },
        { type: "repo", source: "git@github.com:org/repo.git" },
      ],
    });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[multi]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    // The hand-rolled ternary never calls compositionSummary at all, so this
    // text can only appear once the row calls the shared sourceSubLine helper.
    expect(await screen.findByText("2 dirs · 1 repo")).toBeInTheDocument();
  });

  // "onboarding is a convenience, not a gate" (add-workspace-dialog.tsx): the
  // step's list is simple — name, source, status — with no per-row Scan/Edit
  // affordance now that the wizard those buttons opened is retired.
  it("an onboarded workspace's row shows its status chip and no Scan/Edit affordance", async () => {
    const scanned = ws({ status: "scanned" });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[scanned]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(await screen.findByText("demo-workspace")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^scan$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /edit workspace…/i })).not.toBeInTheDocument();
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

  // The LaunchStep tests lived here. The step was cut: its "Example — not live
  // config" card showed a fabricated task against a repo that may never have
  // been onboarded, and its lede still advertised the deleted AI Run Composer.
  // Review is the funnel's last step now and launching is the top bar's job.

});
