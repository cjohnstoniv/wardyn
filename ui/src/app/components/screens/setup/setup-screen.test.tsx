/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
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

// SetupScreen's tree spans the setup/secrets/health/compose/workspaces/policies/
// runs/integrations domains (orchestrator + NewRunDialog + step bodies, incl.
// the embedded IntegrationsScreen); mock each.
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
    // The orchestrator's own count derivation AND the embedded IntegrationsScreen
    // (its own independent fetch) both GET the site config — default to the
    // unconfigured zero value.
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
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a), scanWorkspace: vi.fn() },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() },
}));
vi.mock("../../../lib/api/runs", () => ({
  // The Demos step's DemoRunner calls getRun (reload re-attach) + killRun (end) in
  // addition to createRun; stub all three so the lazily-loaded step mounts cleanly.
  runs: { createRun: vi.fn(), getRun: vi.fn(), killRun: vi.fn() },
}));
// The Demos step embeds AttachTerminal (xterm) + LiveApprovals; neither renders in
// jsdom. Stub them to trivial nodes so the step's body mounts without a real PTY.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: () => null,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: () => null,
}));

import { SetupScreen, setupDismissed, dismissSetup } from "./setup-screen";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";

// The Integrations step embeds IntegrationsScreen, and its own "Manage in
// Integrations" link both call useNavigate() — every render needs a Router
// ancestor now (react-router throws without one), which SetupScreen didn't
// require before this step existed.
function renderScreen(ui: Parameters<typeof render>[0]) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

// E2 provenance is additive/optional. The substrate map (in the shared default)
// names the concrete runtime each LIVE tier runs as; ready barrier cards render
// it. This suite's own pin is its `checks` array (gvisor/loopback/kvm/macos-kvm),
// reused across the review-step assertions below.
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

describe("setupDismissed()/dismissSetup() — localStorage flag", () => {
  beforeEach(() => localStorage.clear());

  it("round-trips through localStorage", () => {
    expect(setupDismissed()).toBe(false);
    dismissSetup();
    expect(setupDismissed()).toBe(true);
  });
});

// 20s suite default, not vitest's 5s: every test here renders the full SetupScreen
// (a heavy tree) and several walk multiple funnel steps with real user-event
// clicks and lazily-loaded step bodies. They run 1.5-4.5s locally but flake
// against the 5s default on a loaded CI runner — the walks are the point of the
// tests, so give the whole suite headroom rather than chasing individual timeouts.
describe("SetupScreen", { timeout: 20_000 }, () => {
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
    // The orchestrator's own SiteConfig read AND the embedded IntegrationsScreen's
    // independent one both GET on mount (unconfigured zero value by default).
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
    renderScreen(<SetupScreen onDone={() => {}} />);

    expect(await screen.findByText(/couldn.t reach wardyn/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-check/i })).toBeInTheDocument();
    // None of the step machinery renders from the made-up fields.
    expect(screen.queryByText(/no sandbox runner/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /pick your barrier/i })).not.toBeInTheDocument();
  });

  it("walks all ten funnel steps and Next/Back move within bounds", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);

    // Walk via the footer `Next: {label}` button (accessible name starts "Next:").
    // The Back button is anchored as /^back$/i so it can't collide with another
    // Back-ish verb. STEP_ORDER (9 -> 10: Corporate network came back): essentials
    // [environment, corp_network, integrations] → demos (four sub-steps) → your
    // work [workspaces] → finish.

    // environment (first) step — barrier-led; the tier cards render, the
    // cross-cutting checks do NOT (they moved to the Review step).
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.getByText("Fence")).toBeInTheDocument();
    expect(screen.queryByText("gVisor runtime")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^back$/i })).toBeDisabled();

    // Corporate network directly follows Environment (the order itself is the
    // fix for "blocked network reads as bad credential" — see steps.ts).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByRole("heading", { name: /^corporate network$/i })).toBeInTheDocument();

    // Integrations follows Corporate network — it folds in the model/SCM-host
    // picker (host proxy / egress redirection moved to the step just visited).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i }),
    ).toBeInTheDocument();

    // The four Demos sub-steps — each renders one demo (heading = its title). The
    // first also proves the lazily-loaded detail body mounts (its setup section).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();
    expect(await screen.findByText(/set up a sandbox like this yourself/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByRole("heading", { name: /fail, then approve/i })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByRole("heading", { name: /held at the door/i })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /lines that can't be crossed/i }),
    ).toBeInTheDocument();

    // your work: workspaces.
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText(/somewhere to work/i)).toBeInTheDocument(); // workspaces

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

  it("the Integrations step embeds the real list (not a second copy) and the rail badge counts a connection", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");

    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
    expect(
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i }),
    ).toBeInTheDocument();
    // The embedded list's own category section — proof it's IntegrationsScreen
    // rendering, not a hand-rolled duplicate (see integrations-screen.test.tsx
    // for that component's own coverage).
    expect(await screen.findByText("AI providers")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /manage in integrations/i })).toBeInTheDocument();

    const nav = screen.getByRole("navigation", { name: /setup steps/i });
    const btn = within(nav).getByRole("button", { name: /integrations/i });
    expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
  });

  // A4 — "Skipped" state: an optional step the operator navigated past without
  // configuring it reads "Skipped" in the rail instead of a perpetual "Optional".
  // Exercised via Integrations now that the old corporate-network steps (which
  // used to carry this coverage) are folded into it.
  describe("A4 — Skipped state", () => {
    it("navigating past Integrations without connecting anything marks its rail badge Skipped", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations

      const nav = screen.getByRole("navigation", { name: /setup steps/i });
      const btn = within(nav).getByRole("button", { name: /integrations/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
      expect(within(btn).queryByText("Optional")).not.toBeInTheDocument();
    });

    it("a connected integration never shows Skipped, even after navigating past it", async () => {
      getSetupStatusMock.mockResolvedValue(
        baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
      );
      listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations

      const nav = screen.getByRole("navigation", { name: /setup steps/i });
      const btn = within(nav).getByRole("button", { name: /integrations/i });
      expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
      expect(within(btn).queryByText("Skipped")).not.toBeInTheDocument();
    });

    it("an explicit 'Skip this step' click earns the checkmark and advances past Integrations", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i });
      await user.click(await screen.findByRole("button", { name: /^skip this step$/i }));

      expect(await screen.findByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();
      const nav = screen.getByRole("navigation", { name: /setup steps/i });
      const btn = within(nav).getByRole("button", { name: /integrations/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });

    it("visited-step tracking round-trips through localStorage across a remount", async () => {
      const { unmount } = renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect what's outside wardyn/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations
      unmount();

      // A fresh mount reads the persisted visited-set back — this must be real
      // localStorage, not in-memory React state that a remount would lose.
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      const nav = screen.getByRole("navigation", { name: /setup steps/i });
      const btn = within(nav).getByRole("button", { name: /integrations/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });
  });

  it("'Finish setup' at the end of the flow dismisses setup and calls onDone (no early exit)", async () => {
    const onDone = vi.fn();
    renderScreen(<SetupScreen onDone={onDone} />);
    await screen.findByText("Fence");

    // No early escape any more — the mandatory gate keeps the operator in setup.
    expect(screen.queryByRole("button", { name: /finish later/i })).not.toBeInTheDocument();

    // Jump to the final (Launch) step via the rail and complete via "Finish setup".
    await user.click(screen.getByRole("button", { name: /^Launch —/ }));
    await user.click(screen.getByRole("button", { name: /^finish setup$/i }));
    expect(setupDismissed()).toBe(true);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  // The fast-path banner was REMOVED (it duplicated the Launch step and talked
  // over the step being configured). Launching early still works — from the
  // Launch step — so that behavior keeps its coverage here.
  it("no fast-path banner renders even when ready with a model connected", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: true,
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
      }),
    );
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // render settled
    expect(screen.queryByText(/you're ready — launch your first run now/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /keep setting up/i })).not.toBeInTheDocument();
  });

  it("the Launch step's CTA opens the run dialog", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        ready: true,
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
      }),
    );
    renderScreen(<SetupScreen onDone={() => {}} />);
    await user.click(await screen.findByRole("button", { name: /^Launch —/ }));
    await user.click(
      (await screen.findAllByRole("button", { name: /^launch your first run$/i }))[0],
    );
    // The composer is off in this build, so the dialog opens straight into the
    // manual wizard — assert on its dialog description.
    expect(
      await screen.findByText(/compose the agent's permission envelope/i),
    ).toBeInTheDocument();
  });

  it("review step renders ok/warn/fail/info rows grouped, and Re-check calls getSetupStatus again", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // environment settled
    // walk to Review (step 9 of 10) — checks live there now, not the barrier step
    for (let i = 0; i < 8; i++) await user.click(screen.getByRole("button", { name: /^next:/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument(); // ok (Ready group)
    expect(screen.getByText("Loopback bind")).toBeInTheDocument(); // warn (Worth a look)
    expect(screen.getByText("/dev/kvm")).toBeInTheDocument(); // fail (Blocking)
    expect(screen.getByText("macOS note")).toBeInTheDocument(); // info (Ready group)
    // the fail row's client-absent fix falls through to the backend-provided fix
    expect(screen.getByText(/enable virtualization/i)).toBeInTheDocument();
    // grouped headings prove the rollup, not a flat dump
    expect(screen.getByText("Blocking")).toBeInTheDocument();

    // 1 from the orchestrator's own mount + 1 from the embedded IntegrationsScreen's
    // own independent fetch when the walk passed through the Integrations step
    // (its FIRST load never cascades into a caller recheck — see
    // integrations-screen.tsx's loadedOnceRef).
    expect(getSetupStatusMock).toHaveBeenCalledTimes(2);
    // Two Re-check buttons now share this step: the persistent HostStatusBar (first
    // in DOM order) and ReviewStep's own (rendered after it in the step body). Both
    // invoke the same re-check; click the ReviewStep's own and assert getSetupStatus
    // is called again.
    const rechecks = screen.getAllByRole("button", { name: /re-check/i });
    expect(rechecks).toHaveLength(2);
    await user.click(rechecks[rechecks.length - 1]);
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(3));
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
    renderScreen(<SetupScreen onDone={() => {}} />);
    // Barrier step: the 3-tier runner list renders; the cross-cutting checks do NOT.
    expect(await screen.findByText("Fence")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    expect(screen.queryByText("Secret store durability")).not.toBeInTheDocument();
    // Walk to Review: the non-platform check appears grouped; the platform note under "About this host".
    for (let i = 0; i < 8; i++) await user.click(screen.getByRole("button", { name: /^next:/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("Secret store durability")).toBeInTheDocument();
    expect(screen.getByText("About this host")).toBeInTheDocument();
    expect(screen.getByText("WSL networking")).toBeInTheDocument();
  });

  // E2 — setup-check provenance ------------------------------------------------

  it("environment step names the concrete substrate each ready tier runs as (E2)", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
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

  // E3 — default barrier tier selection ---------------------------------------

  it("preselects the resolved default barrier, persists a click, and keeps a todo card's setup command working (E3)", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
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
    renderScreen(<SetupScreen onDone={() => {}} />);
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
    renderScreen(<SetupScreen onDone={() => {}} />);
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
});
