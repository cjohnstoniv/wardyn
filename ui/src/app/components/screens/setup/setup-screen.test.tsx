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

function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [
      { id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" },
      { id: "loopback", label: "Loopback bind", status: "warn", detail: "bound to 0.0.0.0" },
      { id: "kvm", label: "/dev/kvm", status: "fail", detail: "missing", fix: "enable virtualization" },
      { id: "macos-kvm", label: "macOS note", status: "info", detail: "CC3 unavailable on macOS" },
    ],
    auth: { mode: "local", local_loopback: true },
    // E2 provenance is additive/optional. The substrate map — the one E2 field with
    // a home in the base fixture — names the concrete runtime each LIVE tier runs
    // as; ready barrier cards render it. (auth_mode needs a logged-in CLI and
    // composer transport/auth need a backend, neither of which the base carries, so
    // those two are exercised in the dedicated E2 provider test below, not here.)
    runner: {
      driver: "docker",
      confinement_classes: ["CC1", "CC2"],
      confinement_substrates: { CC1: "oci/runc", CC2: "oci/runsc" },
    },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
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

  it("walks all nine funnel steps and Next/Back move within bounds", async () => {
    render(<SetupScreen onDone={() => {}} />);

    // environment (first) step — barrier-led; the tier cards render, the
    // cross-cutting checks do NOT (they moved to the Review step).
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.getByText("Fence")).toBeInTheDocument();
    expect(screen.queryByText("gVisor runtime")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /back/i })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText("Claude / Anthropic")).toBeInTheDocument(); // provider (family group)

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /corporate host proxy/i }),
    ).toBeInTheDocument(); // host_proxy

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /source control provider/i }),
    ).toBeInTheDocument(); // scm_provider

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /artifact registry redirection/i }),
    ).toBeInTheDocument(); // artifact_repo

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText(/somewhere to work/i)).toBeInTheDocument(); // workspaces

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText("GitHub App")).toBeInTheDocument(); // credentials

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    // review step — the consolidated readiness rollup + the checks that used to
    // live on the barrier step (now grouped, e.g. the "gVisor runtime" ok row).
    expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    // launch step — the example config + the launch button
    expect(
      await screen.findByRole("button", { name: /launch your first run/i }),
    ).toBeInTheDocument();
    // last step: no more Next
    expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();

    // Back from launch lands on Review (the new penultimate step).
    await user.click(screen.getByRole("button", { name: /back/i }));
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
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> provider
    await screen.findByText("Claude / Anthropic");
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
    // Every check the backend reports for these three ids stays "info" (never
    // "ok") — the rail badge must not depend on that to say "Configured".
    const hostProxyBtn = await screen.findByRole("button", { name: /host proxy/i });
    const scmBtn = screen.getByRole("button", { name: /scm provider/i });
    const artifactBtn = screen.getByRole("button", { name: /artifact redirect/i });
    expect(within(hostProxyBtn).getByText("Configured")).toBeInTheDocument();
    expect(within(scmBtn).getByText("Configured")).toBeInTheDocument();
    expect(within(artifactBtn).getByText("Configured")).toBeInTheDocument();
  });

  it("still shows Optional for the corporate-baseline steps with no SiteConfig set", async () => {
    // beforeEach's getSiteConfigMock already resolves {} — nothing configured.
    render(<SetupScreen onDone={() => {}} />);

    await screen.findByText("Fence");
    const hostProxyBtn = await screen.findByRole("button", { name: /host proxy/i });
    const scmBtn = screen.getByRole("button", { name: /scm provider/i });
    const artifactBtn = screen.getByRole("button", { name: /artifact redirect/i });
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

  it("the fast path appears only when the host reports ready, and jumps to launch", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ ready: true, providers: [{ tool: "claude", installed: true, logged_in: true }] }),
    );
    render(<SetupScreen onDone={() => {}} />);
    expect(await screen.findByText(/you're ready — launch your first run now/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^launch your first run$/i }));
    // jumped to the launch step — the example config's "Example — not live config" marker
    expect(await screen.findByText(/not live config/i)).toBeInTheDocument();
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
    await user.click(screen.getByRole("button", { name: /re-check/i }));
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

  it("provider step nudges a personal compose box toward host mode; host mode never sees it", async () => {
    // compose + local box, model undetected -> the "make setup-host" nudge shows
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        deployment: { host_like: false },
        providers: [{ tool: "claude", installed: false, logged_in: false }],
      }),
    );
    const { unmount } = render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // provider
    expect(await screen.findByText("make setup-host")).toBeInTheDocument();
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
    expect(screen.queryByText("make setup-host")).not.toBeInTheDocument();
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

  // E2 — setup-check provenance ------------------------------------------------

  it("environment step names the concrete substrate each ready tier runs as (E2)", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // render settled
    // baseStatus runner has CC1+CC2 ready with a substrate map; each ready card
    // shows "Running here as <substrate>". Vault (CC3) is todo here (no substrate),
    // so exactly the two ready tiers carry the line.
    expect(screen.getByText("Running here as oci/runc")).toBeInTheDocument();
    expect(screen.getByText("Running here as oci/runsc")).toBeInTheDocument();
    expect(screen.getAllByText(/Running here as/)).toHaveLength(2);
  });

  it("provider step surfaces the LLM auth mode + composer backend transport/auth (E2)", async () => {
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
        // sentence (fresh vs EXPIRED, inject on/off); the Claude subscription row
        // renders it verbatim so the two rows can never disagree.
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

    // cliRow renders the llm_provider detail verbatim (auth-mode aware).
    expect(
      await screen.findByText("Claude Code CLI w/ Claude subscription — token valid"),
    ).toBeInTheDocument();
    // ComposerBackends appends the muted transport/auth provenance.
    expect(screen.getByText("· api")).toBeInTheDocument();
    expect(screen.getByText("· apikey")).toBeInTheDocument();
  });

  // E3 — default barrier tier selection ---------------------------------------

  it("preselects the resolved default barrier, persists a click, and keeps a todo card's setup command working (E3)", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    // Ready cards are selectable <button>s (aria-pressed); todo/unavailable cards
    // stay plain <div>s. baseStatus has CC1+CC2 ready and no persisted pick, so the
    // resolved default is the strongest available (Wall) — the SOLE pressed card.
    expect(screen.getAllByRole("button", { pressed: true })).toHaveLength(1);
    expect(
      screen.getByRole("button", { name: /Real work on real repos/, pressed: true }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Trying Wardyn out/, pressed: false }),
    ).toBeInTheDocument();

    // Clicking the Fence card moves the selection AND persists it to localStorage.
    await user.click(screen.getByRole("button", { name: /Trying Wardyn out/ }));
    expect(getDefaultCc()).toBe("CC1");
    expect(screen.getAllByRole("button", { pressed: true })).toHaveLength(1);
    expect(
      screen.getByRole("button", { name: /Trying Wardyn out/, pressed: true }),
    ).toBeInTheDocument();

    // Regression the ready-only wrapper decision exists to prevent: Vault (CC3) is
    // a todo card — a plain <div> — so its "Show setup command" button still toggles
    // the inline command instead of being swallowed by a selection <button>.
    expect(screen.queryByText("wardyn setup vault")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /show setup command/i }));
    expect(screen.getByText("wardyn setup vault")).toBeInTheDocument();
    // Toggling the todo card's command never disturbs the barrier selection.
    expect(screen.getAllByRole("button", { pressed: true })).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: /hide setup command/i }));
    expect(screen.queryByText("wardyn setup vault")).not.toBeInTheDocument();
  });

  // Barrier taxonomy — incompatible (hardware) vs needs-setup (installable) ----

  it("recommends the strongest COMPATIBLE tier: missing Vault on KVM hardware is Needs setup, still Recommended", async () => {
    render(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    // baseStatus: CC1+CC2 ready, CC3 missing, platform.kvm=true — Vault is a
    // fixable gap, never a dead end: the single Recommended chip sits on Vault
    // (whose status reads Needs setup), not on the weaker currently-ready Wall.
    const recommended = screen.getAllByText("Recommended");
    expect(recommended).toHaveLength(1);
    expect(recommended[0].closest('[class*="rounded-xl"]')?.textContent).toContain("Vault");
    expect(screen.queryByText("Incompatible here")).not.toBeInTheDocument();
    // The selection ring (the ACTUAL default for new runs) stays on ready tiers.
    expect(
      screen.getByRole("button", { name: /Real work on real repos/, pressed: true }),
    ).toBeInTheDocument();
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
    expect(recommended[0].closest('[class*="rounded-xl"]')?.textContent).toContain("Wall");
  });
});
