/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke-level only: each step body renders with minimal fixtures and shows one
// signature element; the deep orchestrator assertions live in
// setup-screen.test.tsx. Mocking conventions mirror setup-screen.test.tsx (same
// api-module mock shape, same baseStatus()).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus, SiteConfig } from "../../../lib/types";

const getSetupStatusMock = vi.fn();
const listSecretsMock = vi.fn();
const setSecretMock = vi.fn();
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
    telemetry: vi.fn().mockResolvedValue(undefined),
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

import {
  HostProxyStep,
  ScmProviderStep,
  ArtifactRepoStep,
  WorkspacesStep,
  CredentialsStep,
  ReviewStep,
  LaunchStep,
} from "./step-bodies";
import { ModelStep } from "./llm-access";
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

// V2: the corp steps (Host Proxy / SCM Provider / Artifact Redirect) no longer
// own their own SiteConfig fetch — the orchestrator does, and hands down
// siteConfig + reloadSiteConfig/saveSiteConfig. Fresh mocks per call so a test
// asserting on saveSiteConfig doesn't inherit another test's call history.
function siteConfigProps(cfg: SiteConfig | null = {}) {
  return {
    siteConfig: cfg,
    reloadSiteConfig: vi.fn().mockResolvedValue(undefined),
    saveSiteConfig: vi.fn().mockResolvedValue(undefined),
  };
}

describe("step-bodies.tsx — smoke", () => {
  beforeEach(() => {
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2"] });
    listComposerBackendsMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    scanWorkspaceMock.mockReset().mockResolvedValue({ async: false });
  });

  it("HostProxyStep renders its upstream-proxy-secret field", async () => {
    render(
      <HostProxyStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(await screen.findByText("Upstream proxy secret name")).toBeInTheDocument();
  });

  it("HostProxyStep's Add-secret button opens the flow inline (no dead cross-step pointer)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onAddSecret = vi.fn();
    render(
      <HostProxyStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={onAddSecret}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    await user.click(await screen.findByRole("button", { name: /add secret/i }));
    // Empty field falls back to the conventional name.
    expect(onAddSecret).toHaveBeenCalledWith("upstream-proxy-url");
  });

  it("ScmProviderStep renders its per-host-credential card", async () => {
    render(
      <ScmProviderStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(await screen.findByText("Fine-grained PAT")).toBeInTheDocument();
  });

  // Safest-path ladder (Change 1): the four rungs render in order, GitHub App
  // first and badged Recommended — never re-sorted by configured/detected state.
  it("ScmProviderStep renders the GitHub App card first, badged Recommended", async () => {
    render(
      <ScmProviderStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    const headings = await screen.findAllByRole("heading", { level: 3 });
    expect(headings.map((h) => h.textContent)).toEqual([
      "GitHub App",
      "Fine-grained PAT",
      "SSH deploy key",
    ]);
    expect(screen.getByText("Recommended")).toBeInTheDocument();
  });

  // The GitHub App card is deliberately a status+pitch card (its App ID / PEM form
  // lives on the Credentials step) — but it badges "Needs setup", so it must offer a
  // way THERE. Without this it is a dead end: an orange badge with nothing to click,
  // while every other rung on the step has an action.
  it("ScmProviderStep's GitHub App card jumps to the Credentials step when unconfigured", async () => {
    const user = userEvent.setup();
    const onJump = vi.fn();
    render(
      <ScmProviderStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={onJump}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    await user.click(await screen.findByRole("button", { name: /set up on the credentials step/i }));
    expect(onJump).toHaveBeenCalledWith("credentials");
  });

  it("ScmProviderStep hides the GitHub App jump once the App is configured", async () => {
    render(
      <ScmProviderStep
        status={baseStatus({ secrets: { present: [], github_app: true } })}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(await screen.findByText("GitHub App")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /set up on the credentials step/i }),
    ).not.toBeInTheDocument();
  });

  it("ScmProviderStep shows the gh-CLI posture line only when scm.gh_cli is true", async () => {
    const { rerender } = render(
      <ScmProviderStep
        status={baseStatus()}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.queryByText(/gh CLI login detected/i)).not.toBeInTheDocument();

    rerender(
      <ScmProviderStep
        status={baseStatus({
          scm: { gh_cli: true, credential_helper: "", git_credentials_file: false, netrc: false },
        })}
        {...siteConfigProps()}
        onAddSecret={vi.fn()}
        onJump={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(await screen.findByText(/gh CLI login detected on the host/i)).toBeInTheDocument();
  });

  it("ArtifactRepoStep renders its ecosystem field", async () => {
    render(
      <ArtifactRepoStep status={baseStatus()} {...siteConfigProps()} onRecheck={vi.fn()} rechecking={false} />,
    );
    expect(await screen.findByText("Ecosystem")).toBeInTheDocument();
  });

  it("WorkspacesStep renders the empty-state onboard affordance", () => {
    render(<WorkspacesStep workspaces={[]} loading={false} onReload={vi.fn()} />);
    expect(screen.getByText("No workspaces onboarded yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add workspace|onboard your first workspace/i })).toBeInTheDocument();
  });

  it("CredentialsStep renders the GitHub App card", () => {
    render(
      <CredentialsStep status={baseStatus()} onAddSecret={vi.fn()} onRecheck={vi.fn()} rechecking={false} />,
    );
    expect(screen.getByText("GitHub App")).toBeInTheDocument();
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

  // V2: a successful save PUTs through the orchestrator-owned saveSiteConfig
  // (the single SiteConfig owner) instead of the step's own local hook.
  it("HostProxyStep saves via the orchestrator-owned saveSiteConfig", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const saveSiteConfig = vi.fn().mockResolvedValue(undefined);
    render(
      <HostProxyStep
        status={baseStatus()}
        {...siteConfigProps()}
        saveSiteConfig={saveSiteConfig}
        onAddSecret={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    const input = await screen.findByPlaceholderText("upstream-proxy-url");
    await user.type(input, "corp-proxy");
    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    await waitFor(() => expect(saveBtn).toBeEnabled());
    await user.click(saveBtn);
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith({ upstream_proxy_secret_ref: "corp-proxy" }),
    );
  });

  it("ModelStep renders 'Refresh detection' and never the composer-BACKENDS config UI", () => {
    const status = baseStatus();
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.getByText("Refresh detection")).toBeInTheDocument();
    // The composer-backends CONFIG UI was dropped by owner decision (LLM access
    // only). Referencing the AI Run Composer in prose as a REASON to connect a
    // model is fine; what must never appear is the backend-config surface.
    expect(screen.queryByText(/composer backend/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /composer/i })).not.toBeInTheDocument();
  });
});
