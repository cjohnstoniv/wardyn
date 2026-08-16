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
import { cleanup, render, screen } from "@testing-library/react";
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
vi.mock("../../../lib/api/sources", () => ({
  sourcesApi: { listSources: () => Promise.resolve([]) },
  baseImagesApi: { listBaseImages: () => Promise.resolve([]) },
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

import { ImagesStep, LaunchStep, ReviewStep, SourcesStep, WorkspacesStep } from "./step-bodies";
import { deriveReadiness } from "../../../lib/readiness";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";
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
    listComposerBackendsMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    scanWorkspaceMock.mockReset().mockResolvedValue({ async: false });
  });

  it("SourcesStep and ImagesStep render their tier libraries as their own step bodies", async () => {
    render(
      <MemoryRouter>
        <SourcesStep workspaces={[]} />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("button", { name: /add directory or repo/i })).toBeInTheDocument();
    cleanup();
    render(
      <MemoryRouter>
        <ImagesStep workspaces={[]} />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("button", { name: /add base image/i })).toBeInTheDocument();
  });

  it("WorkspacesStep renders the empty-state onboard affordance, which opens the four-step wizard (origin=setup)", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText("No workspaces onboarded yet.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /onboard your first workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
    expect(screen.getAllByText("Sources").length).toBeGreaterThan(0);
    // origin="setup": Done's primary action is "Back to setup", not "Open …→".
    // (Not reached by this smoke test — the step rail assertion above is the
    // load-bearing check that THIS wizard, not the retired panel, is mounted.)
  });

  // UX-4: the row used to re-implement the single-source sub-line by hand
  // ({kind === "repo" ? "repo" : "local dir"} · {w.source}), so a multi-source
  // workspace — this range's own headline shape — rendered "local dir ·" with
  // a blank path. sourceSubLine (workspaces.tsx) is the one helper the detail
  // page's header already reuses for exactly this reason.
  it("a multi-source workspace's row renders the composition summary via sourceSubLine, not a hand-rolled single-source line", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
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

  // UI-SETUP-4 + UX-3: the funnel's only per-row affordances used to be
  // Scan (isUsable) XOR Open (navigate to /workspaces/:id — a route the
  // mandatory first-run gate immediately bounces back to /setup, restarting
  // the funnel and discarding the in-memory corp-network proof). Neither
  // branch could resume a workspace whose wizard was closed part-way. "Edit
  // workspace…" now opens the SAME wizard component /workspaces uses,
  // in-dialog — no navigation, no new route, no gate change.
  it("'Edit workspace…' opens the wizard in-dialog with `initial` set — never navigates to a gated route", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
    const notUsable = ws({ id: "ws-pending", name: "still-scanning", status: "pending_scan" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[notUsable]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    // Not-yet-usable: no "Scan" (nothing to re-scan yet), but Edit workspace…
    // is there regardless of status.
    expect(screen.queryByRole("button", { name: /^scan$/i })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /edit workspace…/i }));
    expect(await screen.findByRole("heading", { name: "Edit workspace" })).toBeInTheDocument();
  });

  // UX-3: a workspace already past pending_scan (isUsable) — the state a
  // wizard closed after kicking its scan settles to — used to offer ONLY
  // "Scan" (a re-scan), no way back into the wizard to finish base
  // image/requirements/verify. Edit workspace… stands beside Scan here, not
  // instead of it.
  it("a usable-but-unfinished workspace gets 'Edit workspace…' ALONGSIDE Scan, not instead of it", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
    const scannedNoImage = ws({ status: "scanned" }); // isUsable; initialStepFor lands on "image"
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[scannedNoImage]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("button", { name: /^scan$/i })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /edit workspace…/i }));
    expect(await screen.findByRole("heading", { name: "Edit workspace" })).toBeInTheDocument();
  });

  // UI-SETUP-12: "Resume import" was the guided-import panel's own control —
  // deleted with that panel. The inline failure note kept telling operators
  // to click a button that exists nowhere in the console; the row's actual
  // button is Edit workspace… (was "Open", same UI-SETUP-4 bounce).
  it("a failed scan's inline note points at opening the workspace, not the deleted 'Resume import' control", async () => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    const failed = ws({ id: "ws-failed", name: "broken-repo", status: "error" });
    render(
      <MemoryRouter>
        <WorkspacesStep workspaces={[failed]} loading={false} onReload={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText(/scan failed — open the workspace to see what went wrong and retry\./i)).toBeInTheDocument();
    expect(screen.queryByText(/resume import/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /resume import/i })).not.toBeInTheDocument();
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
