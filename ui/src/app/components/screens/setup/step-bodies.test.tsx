/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke-level only: each step body renders with minimal fixtures and shows one
// signature element; the deep orchestrator assertions live in
// setup-screen.test.tsx. Mocking conventions mirror setup-screen.test.tsx (same
// api-module mock shape, same baseStatus()). ScmProviderStep and CredentialsStep
// get denser coverage below (Claude Design Page 5/6 redesign) since they're
// operator-facing flows (add/rotate/delete a credential), not just a render smoke.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { useState } from "react";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus, SiteConfig } from "../../../lib/types";

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

import {
  HostProxyStep,
  ArtifactRepoStep,
  WorkspacesStep,
  CredentialsStep,
  ReviewStep,
  LaunchStep,
} from "./step-bodies";
import { ScmProviderStep } from "./scm-provider-step";
import { ModelStep } from "./llm-access";
import { deriveReadiness } from "../onboarding/intro";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";
import { LANE_META } from "../../../lib/scm-provider";

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

// Stateful variant: saveSiteConfig feeds the NEXT render. Asserting on the mock
// alone can't see what a save actually did to the list — the "Remove host"
// no-op (a row rebuilt from the surviving secret's name) is invisible to a stub
// that throws the result away.
function StatefulScmProviderStep({ status, initial }: { status: SetupStatus; initial: SiteConfig }) {
  const [cfg, setCfg] = useState<SiteConfig>(initial);
  return (
    <ScmProviderStep
      status={status}
      siteConfig={cfg}
      reloadSiteConfig={() => Promise.resolve()}
      saveSiteConfig={(next) => {
        setCfg(next);
        return Promise.resolve();
      }}
      onRecheck={vi.fn()}
      rechecking={false}
    />
  );
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

  // ------------------------------------------------------------
  // ScmProviderStep (Claude Design Page 5 redesign) — a per-host provider list
  // + a two-panel Add-provider dialog, replacing the old fixed four-card ladder.
  // Rows are a pure reshaping of status.secrets.present/github_app +
  // siteConfig.scm_hosts (lib/scm-provider.ts's deriveProviders) — there is no
  // provider backend and so no verify/test-connection control anywhere.
  // ------------------------------------------------------------
  describe("ScmProviderStep", () => {
    it("renders the empty state with the honest no-credential sentence and an Add provider button", async () => {
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      expect(await screen.findByText("No providers configured")).toBeInTheDocument();
      expect(screen.getByText("Public repos clone without any credential.")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /add provider/i })).toBeInTheDocument();
      // Honesty canon: nothing server-side exists to verify a provider against.
      expect(screen.queryByRole("button", { name: /verify|test connection/i })).not.toBeInTheDocument();
    });

    it("derives a populated list from secret names, scm_hosts, and github_app", async () => {
      render(
        <ScmProviderStep
          status={baseStatus({
            secrets: {
              present: ["git-pat-dev-azure-com", "github-app-id", "github-app-key"],
              github_app: true,
            },
          })}
          {...siteConfigProps({ scm_hosts: ["dev.azure.com", "gitlab.com"] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      expect(await screen.findByText("GitHub")).toBeInTheDocument();
      expect(screen.getByText("github.com")).toBeInTheDocument();
      expect(screen.getByText(LANE_META.app.label)).toBeInTheDocument();
      expect(screen.getByText("Azure DevOps")).toBeInTheDocument();
      expect(screen.getByText("dev.azure.com")).toBeInTheDocument();
      expect(screen.getByText(LANE_META.pat.label)).toBeInTheDocument();
      expect(screen.getByText("GitLab")).toBeInTheDocument();
      expect(screen.getByText("gitlab.com")).toBeInTheDocument();
      expect(screen.getByText("No credential")).toBeInTheDocument(); // gitlab.com: zero lanes
    });

    it("lists any present LEGACY_NAMES in a footer, never as a first-class provider row", async () => {
      render(
        <ScmProviderStep
          status={baseStatus({ secrets: { present: ["ado-pat"], github_app: false } })}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      // Still the empty state for first-class rows — a legacy name never becomes one.
      expect(await screen.findByText("No providers configured")).toBeInTheDocument();
      expect(screen.getByText("Off-convention names")).toBeInTheDocument();
      expect(screen.getByText("ado-pat")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /re-add under recognized name/i })).toBeInTheDocument();
      // The footer is about NAMING, not usability: a git_pat grant can name any
      // stored secret, so it must never say these do nothing.
      expect(screen.getByText(/still usable/i)).toBeInTheDocument();
    });

    it("deleting an off-convention name warns that a grant may still reference it — never that it is inert", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus({ secrets: { present: ["ado-pat"], github_app: false } })}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /^delete$/i }));

      const dialog = await screen.findByRole("alertdialog");
      expect(within(dialog).getByText(/loses its credential/i)).toBeInTheDocument();
      expect(within(dialog).getByText(/cannot be recovered/i)).toBeInTheDocument();
      expect(dialog.textContent).not.toMatch(/no effect on runs/i);
    });

    it("the Add-provider dialog's panel 1 live-previews the git-pat-<slug> name as a hosted hostname is typed", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /add provider/i }));
      // GHES is a "hosted" pick (no fixed host) — selecting it reveals the Host field.
      await user.click(screen.getByRole("radio", { name: /ghes/i }));
      const hostInput = await screen.findByLabelText(/^host$/i);
      await user.type(hostInput, "Git-Server.corp.com");
      expect(screen.getByText("git-pat-git-server-corp-com")).toBeInTheDocument();

      await user.click(screen.getByRole("button", { name: /^continue$/i }));
      expect(await screen.findByRole("heading", { name: /add provider — ghes/i })).toBeInTheDocument();
      expect(screen.getByText("git-server.corp.com")).toBeInTheDocument();
    });

    it("panel 1 blocks a host the server would reject, before any secret is stored", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /add provider/i }));
      await user.click(screen.getByRole("radio", { name: /ghes/i }));
      const hostInput = await screen.findByLabelText(/^host$/i);

      // validSiteHost requires a dot — panel 2 would have saved the secret and
      // only failed on Done, with a raw 400 that mentions neither.
      await user.type(hostInput, "localhost");
      expect(screen.getByText(/containing a dot/i)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^continue$/i })).toBeDisabled();

      // Shape-valid but the derived name blows the 128-char secret-name cap —
      // and panel 2's Name field is read-only, so that was a dead end.
      await user.clear(hostInput);
      await user.type(hostInput, `${"b".repeat(130)}.example.com`);
      expect(screen.getByText(/limit is 128/i)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^continue$/i })).toBeDisabled();

      await user.clear(hostInput);
      await user.type(hostInput, "ghes.corp.internal");
      expect(screen.getByRole("button", { name: /^continue$/i })).toBeEnabled();
    });

    it("GitHub's ladder is a fixed order — App, then fine-grained PAT, then SSH deploy key — Recommended on the App rung", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /add provider/i }));
      // GitHub is panel 1's default selection.
      await user.click(screen.getByRole("button", { name: /^continue$/i }));
      expect(await screen.findByRole("heading", { name: /add provider — github/i })).toBeInTheDocument();

      const rungTitles = screen.getAllByText(/^(GitHub App|Fine-grained PAT|SSH deploy key)$/);
      expect(rungTitles.map((n) => n.textContent)).toEqual(["GitHub App", "Fine-grained PAT", "SSH deploy key"]);
      expect(screen.getByText("Recommended")).toBeInTheDocument();
      expect(screen.getByText(/grants your whole account/i)).toBeInTheDocument();
    });

    it("the GitHub App rung saves an App ID inline — no jump to another step", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /add provider/i }));
      await user.click(screen.getByRole("button", { name: /^continue$/i }));
      const appIdInput = await screen.findByPlaceholderText(/1284951/);
      await user.type(appIdInput, "998877");
      await user.click(screen.getByRole("button", { name: /^save$/i }));
      await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("github-app-id", "998877"));
      expect(screen.queryByRole("button", { name: /credentials step/i })).not.toBeInTheDocument();
    });

    it("a rung's CTA opens the locked AddSecretDialog with the right name/host/lane", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /add provider/i }));
      await user.click(screen.getByRole("button", { name: /^continue$/i })); // GitHub, default
      await user.click(await screen.findByRole("button", { name: /^add pat$/i })); // rung 2's CTA

      // Two dialogs are stacked now (Add provider behind, the secret dialog on
      // top) — scope inside the secret dialog so its own "PAT · in-sandbox"
      // chip isn't confused with the rung's identical chip behind it.
      const dialog = await screen.findByRole("dialog", { name: /^add secret$/i });
      const nameInput = within(dialog).getByLabelText(/^name$/i);
      expect(nameInput).toHaveValue("git-pat-github-com");
      expect(nameInput).toHaveAttribute("readonly");
      expect(within(dialog).getByText(LANE_META.pat.label)).toBeInTheDocument();

      await user.type(within(dialog).getByLabelText(/value/i), "sk-new-token");
      await user.click(within(dialog).getByRole("button", { name: /save secret/i }));
      await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("git-pat-github-com", "sk-new-token"));
    });

    it("a populated row's kebab rotates or deletes its credential", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus({ secrets: { present: ["git-pat-gitlab-com"], github_app: false } })}
          {...siteConfigProps({ scm_hosts: ["gitlab.com"] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /gitlab.com actions/i }));
      expect(await screen.findByRole("menuitem", { name: /rotate credential/i })).toBeInTheDocument();
      const deleteItem = screen.getByRole("menuitem", { name: /delete credential/i });
      expect(screen.getByRole("menuitem", { name: /remove host/i })).toBeInTheDocument();

      await user.click(deleteItem);
      const confirm = await screen.findByRole("button", { name: /^delete credential$/i });
      await user.click(confirm);
      await waitFor(() => expect(deleteSecretMock).toHaveBeenCalledWith("git-pat-gitlab-com"));
    });

    it("a zero-credential row's kebab offers Add credential, jumping straight to that host's rungs", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: ["gitlab.com"] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /gitlab.com actions/i }));
      await user.click(await screen.findByRole("menuitem", { name: /add credential/i }));
      expect(await screen.findByRole("heading", { name: /add provider — gitlab/i })).toBeInTheDocument();
    });

    it("Remove host on a zero-credential row drops it with no confirm dialog — the row is really gone", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(<StatefulScmProviderStep status={baseStatus()} initial={{ scm_hosts: ["gitlab.com"] }} />);

      await user.click(await screen.findByRole("button", { name: /gitlab.com actions/i }));
      await user.click(await screen.findByRole("menuitem", { name: /remove host/i }));

      // The ROW is gone — not just "saveSiteConfig was called" — and nothing
      // asked for a confirmation on the way.
      await waitFor(() => expect(screen.queryByText("gitlab.com")).not.toBeInTheDocument());
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
      expect(screen.getByText("No providers configured")).toBeInTheDocument();
    });

    it("Remove host on a credentialed row confirms first and says the row stays — because it does", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <StatefulScmProviderStep
          status={baseStatus({ secrets: { present: ["git-pat-gitlab-com"], github_app: false } })}
          initial={{ scm_hosts: ["gitlab.com"] }}
        />,
      );

      await user.click(await screen.findByRole("button", { name: /gitlab.com actions/i }));
      await user.click(await screen.findByRole("menuitem", { name: /remove host/i }));

      const confirm = await screen.findByRole("alertdialog");
      expect(within(confirm).getByText(/this row stays/i)).toBeInTheDocument();
      await user.click(within(confirm).getByRole("button", { name: /^delete scm host$/i }));

      // Exactly what the dialog promised: scm_hosts loses the entry, the row
      // survives — rebuilt from the surviving credential's name, and labelled.
      await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
      expect(screen.getByRole("button", { name: /gitlab.com actions/i })).toBeInTheDocument();
      expect(screen.getByText(/read back from/i)).toBeInTheDocument();
    });

    it("Remove App names both secrets it destroys and deletes both, not just the key", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      render(
        <ScmProviderStep
          status={baseStatus({
            secrets: { present: ["github-app-id", "github-app-key"], github_app: true },
          })}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /github.com actions/i }));
      await user.click(await screen.findByRole("menuitem", { name: /remove app/i }));

      // Two secrets die here; the confirm has to name both (one of them IS the
      // App's private key, and the store is write-only).
      const dialog = await screen.findByRole("alertdialog");
      expect(within(dialog).getByText("github-app-id")).toBeInTheDocument();
      expect(within(dialog).getByText("github-app-key")).toBeInTheDocument();

      await user.click(within(dialog).getByRole("button", { name: /^delete credential$/i }));
      await waitFor(() => {
        expect(deleteSecretMock).toHaveBeenCalledWith("github-app-id");
        expect(deleteSecretMock).toHaveBeenCalledWith("github-app-key");
      });
    });

    it("a half-failed Remove App still rechecks — one secret is gone, the row must stop claiming an App", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      const onRecheck = vi.fn();
      // The PEM delete fails after the App ID delete succeeded: the old
      // Promise.all + onDeleted path skipped the recheck entirely, leaving an
      // "App · brokered" row backed by a half-deleted credential.
      deleteSecretMock.mockImplementation((n: string) =>
        n === "github-app-key" ? Promise.reject(new Error("boom")) : Promise.resolve(),
      );
      render(
        <ScmProviderStep
          status={baseStatus({
            secrets: { present: ["github-app-id", "github-app-key"], github_app: true },
          })}
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={onRecheck}
          rechecking={false}
        />,
      );
      await user.click(await screen.findByRole("button", { name: /github.com actions/i }));
      await user.click(await screen.findByRole("menuitem", { name: /remove app/i }));
      await user.click(await screen.findByRole("button", { name: /^delete credential$/i }));

      await waitFor(() => {
        expect(deleteSecretMock).toHaveBeenCalledWith("github-app-id"); // attempted despite the sibling failure
        expect(onRecheck).toHaveBeenCalled();
      });
    });

    it("shows the gh-CLI posture line only when scm.gh_cli is true", async () => {
      const { rerender } = render(
        <ScmProviderStep
          status={baseStatus()}
          {...siteConfigProps({ scm_hosts: [] })}
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
          {...siteConfigProps({ scm_hosts: [] })}
          onRecheck={vi.fn()}
          rechecking={false}
        />,
      );
      expect(await screen.findByText(/gh CLI login detected on the host/i)).toBeInTheDocument();
    });
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

  // ------------------------------------------------------------
  // CredentialsStep — the GitHub App card moved to SCM Provider's Add dialog,
  // and the PAT quick-add is now DELETED (it was a duplicate door that passed
  // the raw typed host and never registered scm_hosts). What survives: the lead
  // line, the honest per-run binding sentence, and the jump.
  // ------------------------------------------------------------
  describe("CredentialsStep", () => {
    it("is a pointer, not a second credential door — no App card, no quick-add", () => {
      render(<CredentialsStep onJump={vi.fn()} />);
      expect(screen.queryByText("GitHub App")).not.toBeInTheDocument();
      expect(screen.queryByLabelText(/app id/i)).not.toBeInTheDocument();
      expect(screen.queryByText(/quick-add/i)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(/host/i)).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: /go to scm provider/i })).toBeInTheDocument();
    });

    it("states the real binding: a per-run grant, not the secret's name", () => {
      render(<CredentialsStep onJump={vi.fn()} />);
      expect(screen.getByText(/git_pat \/ ssh_key grant/i)).toBeInTheDocument();
    });

    it("the Go to SCM Provider button jumps via onJump", async () => {
      const user = userEvent.setup();
      const onJump = vi.fn();
      render(<CredentialsStep onJump={onJump} />);
      await user.click(screen.getByRole("button", { name: /go to scm provider/i }));
      expect(onJump).toHaveBeenCalledWith("scm_provider");
    });
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
