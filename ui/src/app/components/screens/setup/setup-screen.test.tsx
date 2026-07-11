/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus } from "../../../lib/types";

// api is mocked module-wide (by resolved path), which also covers the wizard's
// (../new-run/wizard.tsx) and NewRunDialog's own "../../../lib/api" imports —
// they resolve to the same physical module from a sibling directory at the
// same depth.
const getSetupStatusMock = vi.fn();
const listSecretsMock = vi.fn();
const setSecretMock = vi.fn();
const healthMock = vi.fn();
const listComposerBackendsMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();

vi.mock("../../../lib/api", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api")>("../../../lib/api");
  return {
    HttpError: actual.HttpError,
    api: {
      getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
      listSecrets: (...a: unknown[]) => listSecretsMock(...a),
      setSecret: (...a: unknown[]) => setSecretMock(...a),
      health: (...a: unknown[]) => healthMock(...a),
      listComposerBackends: (...a: unknown[]) => listComposerBackendsMock(...a),
      listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
      listPolicies: () => Promise.resolve([]),
      // Host Proxy / SCM Provider / Artifact Redirect steps each load the current
      // site config on mount — default to the unconfigured zero value.
      getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
      putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
      createRun: vi.fn(),
      createPolicy: vi.fn(),
      telemetry: vi.fn().mockResolvedValue(undefined),
    },
  };
});

import {
  SetupScreen,
  setupDismissed,
  dismissSetup,
  shouldOpenSetup,
} from "./setup-screen";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";

// E2 provenance is additive/optional. The substrate map (in the shared default)
// names the concrete runtime each LIVE tier runs as; ready barrier cards render
// it. (auth_mode needs a logged-in CLI and composer transport/auth need a
// backend, neither of which the base carries, so those two are exercised in the
// dedicated E2 provider test below, not here.) This suite's own pin is its
// `checks` array (gvisor/loopback/kvm/macos-kvm), reused across the review-step
// assertions below.
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

describe("shouldOpenSetup — pure decision helper", () => {
  it("opens when not ready, not dismissed, and auth is local", () => {
    expect(shouldOpenSetup(baseStatus(), false)).toBe(true);
  });
  it("opens on a fresh control plane (no runs yet) even when ready", () => {
    expect(shouldOpenSetup(baseStatus({ ready: true, has_runs: false }), false)).toBe(true);
  });
  it("stays closed on an established instance (ready, with runs)", () => {
    expect(shouldOpenSetup(baseStatus({ ready: true, has_runs: true }), false)).toBe(false);
  });
  it("stays closed when dismissed", () => {
    expect(shouldOpenSetup(baseStatus(), true)).toBe(false);
  });
  it("stays closed on a hosted/SSO control plane (auth.mode !== local)", () => {
    expect(shouldOpenSetup(baseStatus({ auth: { mode: "sso", local_loopback: false } }), false)).toBe(
      false,
    );
  });
  it("never auto-opens on the unreachable fallback, even with has_runs:false", () => {
    // READY_FALLBACK shape: the daemon didn't answer, every field is synthetic.
    // ready:true alone was NOT enough — the !has_runs branch used to force-open.
    expect(
      shouldOpenSetup(baseStatus({ unreachable: true, ready: true, has_runs: false }), false),
    ).toBe(false);
    expect(shouldOpenSetup(baseStatus({ unreachable: true, ready: false }), false)).toBe(false);
  });
});

describe("setupDismissed()/dismissSetup() — localStorage flag", () => {
  beforeEach(() => localStorage.clear());

  it("round-trips through localStorage", () => {
    expect(setupDismissed()).toBe(false);
    dismissSetup();
    expect(setupDismissed()).toBe(true);
  });
});

describe("SetupScreen", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
    listSecretsMock.mockReset().mockResolvedValue([]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2"] });
    listComposerBackendsMock.mockReset().mockResolvedValue([]);
    // The Workspaces step fetches the onboarded list on mount; without this reset an
    // unmocked vi.fn() returns undefined and .then(setWorkspaces) throws.
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    // The Host Proxy / SCM Provider / Artifact Redirect steps each GET the site
    // config on mount (unconfigured zero value by default).
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
  });

  it("unreachable daemon: shows 'Couldn't reach Wardyn' + Re-check, never the no-runner danger card", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        unreachable: true,
        ready: true,
        runner: { driver: "none", confinement_classes: [] },
        has_runs: false,
      }),
    );
    render(<SetupScreen onDone={() => {}} />);

    expect(await screen.findByText(/couldn.t reach wardyn/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeInTheDocument();
    // None of the step machinery renders from the made-up fields.
    expect(screen.queryByText(/no sandbox runner/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /pick your barrier/i })).not.toBeInTheDocument();
  });

  it("walks all nine funnel steps and Next/Back move within bounds", async () => {
    render(<SetupScreen onDone={() => {}} />);

    // Walk via the footer `Next: {label}` button (accessible name starts "Next:").
    // The Back button is disambiguated as /^back$/i so it doesn't collide with the
    // "Finish later — Come back anytime…" verb. STEP_ORDER: essentials → your
    // work (scm/workspaces/credentials) → corporate network → finish.

    // environment (first) step — barrier-led; the tier cards render, the
    // cross-cutting checks do NOT (they moved to the Review step).
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.getByText("Fence")).toBeInTheDocument();
    expect(screen.queryByText("gVisor runtime")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^back$/i })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText("Claude / Anthropic")).toBeInTheDocument(); // provider (family group)

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /source control provider/i }),
    ).toBeInTheDocument(); // scm_provider (your work starts right after the essentials)

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText(/somewhere to work/i)).toBeInTheDocument(); // workspaces

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText("GitHub App")).toBeInTheDocument(); // credentials

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /corporate host proxy/i }),
    ).toBeInTheDocument(); // host_proxy

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /artifact registry redirection/i }),
    ).toBeInTheDocument(); // artifact_repo

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    // review step — the consolidated readiness rollup + the checks that used to
    // live on the barrier step (now grouped, e.g. the "gVisor runtime" ok row).
    expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    // launch step — its h2 heading + the launch CTA (the footer nav button and
    // LaunchStep's own inline button both read "Launch your first run").
    expect(
      await screen.findByRole("heading", { name: /launch your first run/i }),
    ).toBeInTheDocument();
    expect(
      screen.getAllByRole("button", { name: /^launch your first run$/i }).length,
    ).toBeGreaterThan(0);
    // last step: no more Next
    expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();

    // Back from launch lands on Review (the new penultimate step).
    await user.click(screen.getByRole("button", { name: /^back$/i }));
    expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
  });

  it("host proxy step renders the masked detected-proxy breakdown when present", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        host_proxy: {
          http_proxy: { value: "http://proxy.corp:3128", source: "env", has_credentials: true },
          env_case_mismatch: ["http_proxy"],
          has_credentials: true,
        },
      }),
    );
    render(<SetupScreen onDone={() => {}} />);

    await screen.findByText("Fence");
    // Walk the reordered funnel: provider → your work (scm/workspaces/credentials)
    // → host_proxy. The footer Next label names each stop.
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> provider
    await screen.findByText("Claude / Anthropic");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> scm_provider
    await screen.findByRole("heading", { name: /source control provider/i });
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> workspaces
    await screen.findByText(/somewhere to work/i);
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> credentials
    await screen.findByText("GitHub App");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> host_proxy
    expect(
      await screen.findByRole("heading", { name: /corporate host proxy/i }),
    ).toBeInTheDocument();

    expect(screen.getByText("Detected on this host")).toBeInTheDocument();
    expect(screen.getByText("http://proxy.corp:3128")).toBeInTheDocument();
    expect(screen.getByText("creds")).toBeInTheDocument(); // per-setting has_credentials
    expect(screen.getByText("http_proxy")).toBeInTheDocument(); // env_case_mismatch chip
    expect(screen.getByText("credential")).toBeInTheDocument(); // top-level has_credentials prompt
  });

  // M21: the corporate-baseline step badges used to gate on check.status ===
  // "ok", but the backend hardcodes host_proxy/scm_provider/artifact_repo to
  // "info" forever — so the badge could never read "Configured" even once the
  // operator had actually wired up the matching SiteConfig field. The badge
  // must derive readiness from SiteConfig itself instead.
  it("M21: shows Configured for host proxy / SCM / artifact steps once their SiteConfig field is set", async () => {
    getSiteConfigMock.mockReset().mockResolvedValue({
      upstream_proxy_secret_ref: "corp-proxy",
      scm_hosts: ["git.corp.example.com"],
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.example.com/npm" } },
    });
    render(<SetupScreen onDone={() => {}} />);

    await screen.findByText("Fence");
    // jsdom renders BOTH rail variants (compact icon + full), so scope rail-button
    // queries to the full-rail nav landmark. Host Proxy / Artifact Redirect live in
    // the collapsible "Corporate network" phase, collapsed by default — expand it so
    // their buttons (with their badges) render in the nav.
    const nav = screen.getByRole("navigation", { name: /setup steps/i });
    await user.click(within(nav).getByRole("button", { name: /corporate network/i }));
    // Every check the backend reports for these three ids stays "info" (never
    // "ok") — the rail badge must not depend on that to say "Configured".
    const hostProxyBtn = await within(nav).findByRole("button", { name: /host proxy/i });
    const scmBtn = within(nav).getByRole("button", { name: /scm provider/i });
    const artifactBtn = within(nav).getByRole("button", { name: /artifact redirect/i });
    expect(within(hostProxyBtn).getByText("Configured")).toBeInTheDocument();
    expect(within(scmBtn).getByText("Configured")).toBeInTheDocument();
    expect(within(artifactBtn).getByText("Configured")).toBeInTheDocument();
  });

  it("still shows Optional for the corporate-baseline steps with no SiteConfig set", async () => {
    // beforeEach's getSiteConfigMock already resolves {} — nothing configured.
    render(<SetupScreen onDone={() => {}} />);

    await screen.findByText("Fence");
    // Scope to the full-rail nav (both rail variants render in jsdom) and expand the
    // collapsed "Corporate network" phase so Host Proxy / Artifact Redirect render.
    const nav = screen.getByRole("navigation", { name: /setup steps/i });
    await user.click(within(nav).getByRole("button", { name: /corporate network/i }));
    const hostProxyBtn = await within(nav).findByRole("button", { name: /host proxy/i });
    const scmBtn = within(nav).getByRole("button", { name: /scm provider/i });
    const artifactBtn = within(nav).getByRole("button", { name: /artifact redirect/i });
    expect(within(hostProxyBtn).getByText("Optional")).toBeInTheDocument();
    expect(within(scmBtn).getByText("Optional")).toBeInTheDocument();
    expect(within(artifactBtn).getByText("Optional")).toBeInTheDocument();
  });

  it("'Finish later' (the single exit verb) dismisses setup and calls onDone", async () => {
    const onDone = vi.fn();
    render(<SetupScreen onDone={onDone} />);
    await screen.findByText("Fence");

    await user.click(screen.getByRole("button", { name: /finish later/i }));
    expect(setupDismissed()).toBe(true);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("the fast path appears only when the host reports ready, and its launch opens the run dialog", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ ready: true, providers: [{ tool: "claude", installed: true, logged_in: true }] }),
    );
    render(<SetupScreen onDone={() => {}} />);
    // Banner renders only when genuinely ready AND a model is connected.
    expect(await screen.findByText(/you're ready — launch your first run now/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^launch your first run$/i }));
    // The banner's launch now OPENS NewRunDialog (not a jump to the launch step).
    // The composer is off in this build, so the dialog opens straight into the
    // manual wizard — assert on its dialog description.
    expect(
      await screen.findByText(/compose the agent's permission envelope/i),
    ).toBeInTheDocument();
  });

  it("the fast path is HIDDEN when a barrier is up but no model is connected (honesty)", async () => {
    // ready (a barrier is present) but NO real LLM path: only a fake composer
    // backend, no CLI login, no key. The "you're ready — launch, a model is
    // connected" banner must NOT render (that fallback would fabricate a model).
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: true,
        providers: [{ tool: "claude", installed: true, logged_in: false }],
        composer: {
          enabled: true,
          default: "dev",
          backends: [
            { name: "dev", provider: "fake", model: "demo", wire: "fake", enabled: true, needs_key: false, key_resolved: true },
          ],
        },
        secrets: { present: [], github_app: false },
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // render settled
    expect(screen.queryByText(/you're ready — launch your first run now/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/a model is connected/i)).not.toBeInTheDocument();
  });

  it("review step renders ok/warn/fail/info rows grouped, and Re-check calls getSetupStatus again", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // environment settled
    // walk to Review (step 8 of 9) — checks live there now, not the barrier step
    for (let i = 0; i < 7; i++) await user.click(screen.getByRole("button", { name: /^next:/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument(); // ok (Ready group)
    expect(screen.getByText("Loopback bind")).toBeInTheDocument(); // warn (Worth a look)
    expect(screen.getByText("/dev/kvm")).toBeInTheDocument(); // fail (Blocking)
    expect(screen.getByText("macOS note")).toBeInTheDocument(); // info (Ready group)
    // the fail row's client-absent fix falls through to the backend-provided fix
    expect(screen.getByText(/enable virtualization/i)).toBeInTheDocument();
    // grouped headings prove the rollup, not a flat dump
    expect(screen.getByText("Blocking")).toBeInTheDocument();

    expect(getSetupStatusMock).toHaveBeenCalledTimes(1);
    // Two Re-check buttons now share this step: the persistent HostStatusBar (first
    // in DOM order) and ReviewStep's own (rendered after it in the step body). Both
    // invoke the same re-check; click the ReviewStep's own and assert getSetupStatus
    // is called again.
    const rechecks = screen.getAllByRole("button", { name: /re-check/i });
    expect(rechecks).toHaveLength(2);
    await user.click(rechecks[rechecks.length - 1]);
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(2));
  });

  it("barrier step shows tiers only; the Review step carries the checks + 'About this host'", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        checks: [
          { id: "age_key", label: "Secret store durability", status: "ok", detail: "durable" },
          {
            id: "platform_wsl",
            label: "WSL networking",
            status: "info",
            platform: "wsl",
            detail: "Running under WSL2",
          },
        ],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    // Barrier step: the 3-tier runner list renders; the cross-cutting checks do NOT.
    expect(await screen.findByText("Fence")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    expect(screen.queryByText("Secret store durability")).not.toBeInTheDocument();
    // Walk to Review: the non-platform check appears grouped; the platform note under "About this host".
    for (let i = 0; i < 7; i++) await user.click(screen.getByRole("button", { name: /^next:/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("Secret store durability")).toBeInTheDocument();
    expect(screen.getByText("About this host")).toBeInTheDocument();
    expect(screen.getByText("WSL networking")).toBeInTheDocument();
  });

  it("provider step's Add-key button opens AddSecretDialog", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider

    // Unconfigured Anthropic collapses into the family's "Add Anthropic API key"
    // set-up affordance; clicking it opens the AddSecretDialog.
    await screen.findByText("Claude / Anthropic");
    await user.click(screen.getByRole("button", { name: /add anthropic api key/i }));
    expect(await screen.findByText(/^add secret$/i)).toBeInTheDocument();
  });

  it("provider step nudges a personal compose box toward container login; host mode never sees it", async () => {
    // compose + local box, model undetected -> the container-login nudge shows
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        deployment: { host_like: false },
        providers: [{ tool: "claude", installed: false, logged_in: false }],
      }),
    );
    const { unmount } = render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider
    // The banner now offers container login as the first-class fix (attach works
    // via a minted ticket; the old "coming-soon team feature" copy is gone). The
    // banner's button is the exact-named one; the family option adds a suffix.
    expect(
      await screen.findByRole("button", { name: /^connect via container login$/i }),
    ).toBeInTheDocument();
    unmount();

    // host mode (wardynd sees the login) -> model is connected, no nudge
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        deployment: { host_like: true },
        providers: [{ tool: "claude", installed: true, logged_in: true }],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider
    await screen.findByText("Claude / Anthropic");
    expect(screen.queryByText("coming-soon team feature")).not.toBeInTheDocument();
  });

  // Model/Harness Provider — detection-driven family grouping (owner ask) --------

  it("groups model providers into Claude/Anthropic and OpenAI/Codex families, showing detected rows + set-up affordances", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider

    // Both families render as groups.
    expect(await screen.findByText("Claude / Anthropic")).toBeInTheDocument();
    expect(screen.getByText("OpenAI / Codex")).toBeInTheDocument();
    // Detected (present secret) -> a full configured row for that family.
    expect(screen.getByText("Anthropic API key")).toBeInTheDocument();
    // Undetected in the SAME family stays a compact set-up option, not a full row.
    expect(screen.getByRole("button", { name: /set up aws bedrock/i })).toBeInTheDocument();
    // The OpenAI/Codex family (nothing detected) leads with its set-up options.
    expect(screen.getByRole("button", { name: /add openai api key/i })).toBeInTheDocument();
  });

  // Bedrock (Phase 1B) ---------------------------------------------------------

  it("provider step offers AWS Bedrock as a Set-up option when unconfigured, opening AddSecretDialog prefilled", async () => {
    render(<SetupScreen onDone={() => {}} />); // baseStatus() carries no `bedrock` field at all
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider

    // Unconfigured Bedrock collapses into a contextual set-up button (not a full row).
    await user.click(await screen.findByRole("button", { name: /set up aws bedrock/i }));
    expect(await screen.findByText(/^add secret$/i)).toBeInTheDocument();
    expect(screen.getByDisplayValue("aws-access-key-id")).toBeInTheDocument();
  });

  it("provider step marks AWS Bedrock ready once region/model/creds are all present, echoing them", async () => {
    const base = baseStatus();
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        secrets: { present: ["aws-access-key-id", "aws-secret-access-key"], github_app: false },
        bedrock: { region: "us-east-1", model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", creds_present: true },
        checks: [
          ...base.checks,
          {
            id: "bedrock_provider",
            label: "AWS Bedrock",
            status: "ok",
            detail: "Bedrock is configured (region us-east-1, model us.anthropic.claude-sonnet-4-5-20250929-v1:0) for Claude runs.",
          },
        ],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider

    expect(await screen.findByText("AWS Bedrock (Claude Code)")).toBeInTheDocument();
    expect(screen.getByText(/Bedrock is configured \(region us-east-1/i)).toBeInTheDocument();
    expect(screen.getByText(/us-east-1 · us\.anthropic\.claude-sonnet-4-5-20250929-v1:0/)).toBeInTheDocument();
  });

  it("provider step surfaces the claude_subscription_staging warn verbatim under the subscription row", async () => {
    // The headless-`make setup` skip: login detected (badge green) but NOT staged,
    // so the per-run subscription mount silently does nothing. The check row is
    // the backend's honest signal; the provider step must render it verbatim.
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        providers: [
          { tool: "claude", installed: true, logged_in: true, login_detected_via: "~/.claude/.credentials.json" },
        ],
        checks: [
          {
            id: "claude_subscription_staging",
            label: "Claude subscription staging",
            status: "warn",
            detail: "A resident Claude login was detected — the model-access badge is green — but it is NOT staged for sandbox use.",
            fix: "Run `make stage-claude` on the host.",
          },
        ],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // → provider

    expect(await screen.findByText(/NOT staged for sandbox use/)).toBeInTheDocument();
    expect(screen.getByText(/make stage-claude/)).toBeInTheDocument();
  });

  it("provider step renders no staging note when the staging check is ok or absent", async () => {
    // Staged (ok) => the row is quiet; absent (no login) => nothing. Never a
    // green banner for staging — ok is simply the absence of the warn.
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        providers: [{ tool: "claude", installed: true, logged_in: true }],
        checks: [
          {
            id: "claude_subscription_staging",
            label: "Claude subscription staging",
            status: "ok",
            detail: "Your Claude login is staged for sandbox use.",
          },
        ],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // → provider

    expect(await screen.findByText("Claude / Anthropic")).toBeInTheDocument();
    expect(screen.queryByText(/staged for sandbox use/)).not.toBeInTheDocument();
  });

  // E2 — setup-check provenance ------------------------------------------------

  it("environment step names the concrete substrate each ready tier runs as (E2)", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // render settled
    // baseStatus runner has CC1+CC2 ready with a substrate map; each ready column
    // shows "Running here as <substrate>" — the string spans a text node + a mono
    // <span>, so scope to each tier's column (<th>) and match both parts there.
    // Vault (CC3) is todo (no substrate), so exactly the two ready tiers carry it.
    const fenceCol = screen.getByRole("radio", { name: /Fence/ }).closest("th")!;
    expect(within(fenceCol).getByText(/Running here as/)).toBeInTheDocument();
    expect(within(fenceCol).getByText("oci/runc")).toBeInTheDocument();
    const wallCol = screen.getByRole("radio", { name: /Wall/ }).closest("th")!;
    expect(within(wallCol).getByText(/Running here as/)).toBeInTheDocument();
    expect(within(wallCol).getByText("oci/runsc")).toBeInTheDocument();
    expect(screen.getAllByText(/Running here as/)).toHaveLength(2);
  });

  it("provider step surfaces no composer UI (owner decision)", async () => {
    // Honest inverse of the old composer-provenance test: the ModelStep body
    // deliberately drops the "AI Run Composer backends" section (owner decision:
    // zero composer UI here). Even WITH a composer backend configured, the provider
    // step must render the LLM auth provenance via ModelStep but NO composer text —
    // no transport/auth provenance, no "composer" copy at all.
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        providers: [
          {
            tool: "claude",
            installed: true,
            logged_in: true,
            auth_mode: "subscription",
            login_detected_via: "~/.claude/.credentials.json",
          },
        ],
        // The llm_provider check carries the single authoritative subscription
        // sentence; the Claude subscription row renders it verbatim.
        checks: [
          {
            id: "llm_provider",
            label: "LLM provider",
            status: "ok",
            detail: "Claude Code CLI w/ Claude subscription — token valid",
          },
        ],
        composer: {
          enabled: true,
          default: "prod",
          backends: [
            {
              name: "prod",
              provider: "anthropic",
              model: "sonnet",
              wire: "anthropic",
              transport: "api",
              auth: "apikey",
              enabled: true,
              needs_key: false,
              key_resolved: true,
            },
          ],
        },
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });
    await user.click(screen.getByRole("button", { name: /^next:/i })); // → provider

    // The provider families still render, and the LLM auth line surfaces verbatim.
    expect(await screen.findByText("Claude / Anthropic")).toBeInTheDocument();
    expect(
      screen.getByText("Claude Code CLI w/ Claude subscription — token valid"),
    ).toBeInTheDocument();
    // But the composer-backends section is gone — no transport/auth provenance and
    // no "composer" copy anywhere on the step.
    expect(screen.queryByText(/composer/i)).not.toBeInTheDocument();
    expect(screen.queryByText("· api")).not.toBeInTheDocument();
    expect(screen.queryByText("· apikey")).not.toBeInTheDocument();
  });

  // E3 — default barrier tier selection ---------------------------------------

  it("preselects the resolved default barrier, persists a click, and keeps a todo card's setup command working (E3)", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    // The three tiers are radios (role=radio / aria-checked); the tier name is in
    // each radio's accessible name (Fence/Wall/Vault). baseStatus has CC1+CC2 ready
    // and no persisted pick, so the resolved default is the strongest available
    // (Wall/CC2) — the SOLE checked radio.
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Wall/, checked: true })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Fence/, checked: false })).toBeInTheDocument();

    // Clicking the Fence radio moves the selection AND persists it to localStorage.
    await user.click(screen.getByRole("radio", { name: /Fence/ }));
    expect(getDefaultCc()).toBe("CC1");
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Fence/, checked: true })).toBeInTheDocument();

    // Regression the selectable-only radio decision exists to prevent: Vault (CC3)
    // is a todo tier — its radio is disabled, but its "Show setup command" button
    // still reveals the inline command instead of being swallowed by a selection.
    expect(screen.queryByText(/wardyn setup vault/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /show setup command/i }));
    expect(screen.getByText(/wardyn setup vault/)).toBeInTheDocument();
    // Revealing the todo card's command never disturbs the barrier selection.
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Fence/, checked: true })).toBeInTheDocument();
  });

  // Barrier taxonomy — incompatible (hardware) vs needs-setup (installable) ----

  it("recommends the strongest COMPATIBLE tier: missing Vault on KVM hardware is Needs setup, still Recommended", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    // baseStatus: CC1+CC2 ready, CC3 missing, platform.kvm=true — Vault is a
    // fixable gap, never a dead end: the single Recommended chip sits in the Vault
    // column (whose status reads Needs setup), not on the weaker currently-ready
    // Wall. Scope the chip to the Vault column (<th>) rather than the whole table.
    expect(screen.getAllByText("Recommended")).toHaveLength(1);
    const vaultCol = screen.getByRole("radio", { name: /Vault/ }).closest("th")!;
    expect(within(vaultCol).getByText("Recommended")).toBeInTheDocument();
    expect(screen.queryByText("Incompatible here")).not.toBeInTheDocument();
    // The selection ring (the ACTUAL default for new runs) stays on ready tiers (Wall).
    expect(screen.getByRole("radio", { name: /Wall/, checked: true })).toBeInTheDocument();
  });

  it("marks Vault Incompatible (with the /dev/kvm why) only on a KVM-less host, demoting the recommendation to Wall", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ platform: { os: "linux", wsl: false, kvm: false } }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    expect(screen.getByText("Incompatible here")).toBeInTheDocument();
    expect(screen.getByText(/doesn't expose \/dev\/kvm/)).toBeInTheDocument();
    const recommended = screen.getAllByText("Recommended");
    expect(recommended).toHaveLength(1);
    // Pin the chip to the Wall COLUMN (the old rounded-xl closest() resolved to
    // the whole matrix container, which always contains "Wall" — tautology).
    expect(
      within(screen.getByRole("radio", { name: /Wall/ }).closest("th")!).getByText("Recommended"),
    ).toBeInTheDocument();
  });

  it("hides the fast-path banner when a model is connected but the backend is NOT ready", async () => {
    // Honesty: fastPath requires readiness.ready AND llmReady — a connected
    // model alone (e.g. runner missing) must not fabricate the banner.
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: false,
        providers: [{ tool: "claude", installed: true, logged_in: true }],
      }),
    );
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });
    expect(screen.queryByText(/You're ready — launch your first run now/i)).not.toBeInTheDocument();
  });
});
