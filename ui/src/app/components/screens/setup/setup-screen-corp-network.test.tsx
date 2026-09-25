/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
const listWorkspacesMock = vi.fn();
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const testProxyMock = vi.fn();
const testRedirectMock = vi.fn();

// SetupScreen's tree spans the setup/secrets/health/workspaces/policies/
// runs/integrations domains (orchestrator + NewRunDialog + step bodies, incl.
// the embedded IntegrationsScreen); mock each.
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
    // The orchestrator's own count derivation AND the embedded IntegrationsScreen
    // (its own independent fetch) both GET the site config — default to the
    // unconfigured zero value.
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    // #492: setup-screen.tsx's reloadSiteConfig now reads the ETag-carrying
    // snapshot — routed through the SAME mock these tests already drive.
    getSiteConfigSnapshot: async (...a: unknown[]) => ({ siteConfig: await getSiteConfigMock(...a), etag: null }),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
    // Corporate network's connectivity gate (corp-network-step.tsx) — the
    // walkthroughs below aren't testing the gate itself, so they clear it with
    // one Test-proxy click against the no_runner default (see beforeEach).
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
// The Workspaces step's user-drives card summarises GET /drives (SUPER-only,
// and this suite runs on the operator-context's fail-open default).
const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({
  drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) },
}));
// The `providers` step's own card (setup/providers-card.tsx) and the
// orchestrator's providerCount both GET /workspace-providers — legacy open
// mode (zero rows) by default, like the other unconfigured fixtures above.
const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({
  // The demo step's useDemoRuns calls getRun (reload re-attach) + killRun (end) in
  // addition to createRun; stub all three so the lazily-loaded step mounts cleanly.
  runs: { createRun: vi.fn(), getRun: vi.fn(), killRun: vi.fn() },
}));
// The connection step's cards derive from the SetupStatus/site-config/secrets
// mocks above — there is no separate GET /integrations fetch to stub any more
// (genericIntegrationsApi went with the catalog page it served).
// The Demos step embeds AttachTerminal (xterm) + LiveApprovals; neither renders in
// jsdom. Stub them to trivial nodes so the step's body mounts without a real PTY.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: () => null,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: () => null,
}));

import { SetupScreen } from "./setup-screen";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";

// The Integrations step embeds IntegrationsScreen, and its own "Manage in
// Integrations" link both call useNavigate() — every render needs a Router
// ancestor now (react-router throws without one), which SetupScreen didn't
// require before this step existed.
function renderScreen(ui: Parameters<typeof render>[0], route = "/setup") {
  return render(<MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>);
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
    // The Workspaces step fetches the onboarded list on mount; without this reset an
    // unmocked vi.fn() returns undefined and .then(setWorkspaces) throws.
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    // The orchestrator's own SiteConfig read AND the embedded IntegrationsScreen's
    // independent one both GET on mount (unconfigured zero value by default).
    getSiteConfigMock.mockReset().mockResolvedValue({});
    putSiteConfigMock.mockReset().mockResolvedValue({ siteConfig: {}, danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }); // F6-F6: SiteConfigSaveResult, not void
    getDrivesMock
      .mockReset()
      .mockResolvedValue({ drives: [], grants: [], host_roots_configured: false, runner_target: "docker" });
    // The `providers` step's own card and the orchestrator's providerCount
    // both GET /workspace-providers — legacy open mode (zero rows) by default.
    getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
    // no_runner (this suite mocks no real sandbox runner) is the honest,
    // non-blocking default — see clearCorpNetworkGate for why that's the
    // right fixture for walkthroughs that aren't testing the gate itself.
    testProxyMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "no runner configured, nothing to launch a probe with" });
    testRedirectMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "no runner configured, nothing to launch a probe with" });
  });

  // Corporate network (steps.ts's corpNetworkGate) now requires proof
  // of connectivity before Next unlocks. no_runner is the ONE honest bypass —
  // Wardyn can't probe in this jsdom suite anyway — so a single Test-proxy
  // click clears the gate for every walkthrough below that isn't exercising
  // the gate itself (that coverage lives in corp-network-step.test.tsx and
  // setup-layout.test.tsx).
  // …and steps through the Egress redirection tab: the forward walk passes
  // THROUGH it (navigation, not a gate), so after this helper a single
  // "Next:" click advances to Integrations.
  const clearCorpNetworkGate = async () => {
    await user.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText(/can.t test here/i);
    await user.click(screen.getByRole("button", { name: /^next: egress redirection$/i }));
    await screen.findByText(/nothing redirected on this host/i);
  };

  // The gate itself, wired end to end through the real orchestrator — the unit
  // coverage for each rule lives in steps.test.ts (corpNetworkGate) and
  // corp-network-step.test.tsx (the step body reporting upward); this proves
  // setup-screen.tsx actually connects them.
  // ?step= deep link: the Integrations page's proxy banner hands off to
  // Corporate network with it, now that the step is the only place a proxy is
  // configured (see integrations-screen.tsx's "Open Corporate network").
  describe("Corporate network connectivity gate — wired through the real orchestrator", () => {
    it("the footer's button IS the probe while unproven; one reached probe makes the step done — no tab detour", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^network$/i });

      // Blocked gate with an action: there IS no Next button — the fix-it
      // action stands in its place, under the state's own headline.
      expect(screen.queryByRole("button", { name: /^next: review$/i })).not.toBeInTheDocument();
      expect(screen.getByText("Connectivity isn't proven yet")).toBeInTheDocument();
      // …and exactly ONE launch point on the whole screen: the footer's (the
      // panel's own button is suppressed while the gate row carries it).
      expect(screen.getAllByRole("button", { name: /^test connectivity$/i })).toHaveLength(1);

      testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms", via: "proxy" });
      await user.click(screen.getByRole("button", { name: /^test connectivity$/i }));
      await screen.findByText("Reached · via proxy");

      // Zero redirects + reached = the gate is satisfied (nothing here must
      // be configured), and what the pass unlocks is the step's OTHER tab —
      // the forward walk passes through Egress redirection, not over it.
      const toEgress = await screen.findByRole("button", { name: /^next: egress redirection$/i });
      expect(toEgress).toBeEnabled();
      // The panel's button is back — re-testing stays reachable.
      expect(screen.getByRole("button", { name: /^test again$/i })).toBeInTheDocument();

      // One more click, not a demand: the quiet empty line, then the exit.
      await user.click(toEgress);
      await screen.findByText(/nothing redirected on this host/i);
      // #213 — the gate cleared, and Next now goes STRAIGHT to Review: it is
      // the next (and last) REQUIRED step, not Secrets.
      const next = screen.getByRole("button", { name: /^next: review$/i });
      expect(next).toBeEnabled();
      // Back from here returns to Host proxy — the mirror — without leaving
      // the step; then forward again.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      expect(await screen.findByText("What Wardyn found on this host")).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^next: egress redirection$/i }));

      await user.click(screen.getByRole("button", { name: /^next: review$/i }));
      expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
    });

    it("no_runner unlocks Next immediately with its standing note — nothing was proven, and the note keeps saying so", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^network$/i });

      await clearCorpNetworkGate();
      expect(screen.getByRole("button", { name: /^next: review$/i })).toBeEnabled();
      expect(screen.getByText("Nothing to test with")).toBeInTheDocument();
      expect(screen.getByText(/nothing was proven here/i)).toBeInTheDocument();
    });

    it("configured redirects hold the gate with their own footer action — 'Test the redirect' fires the sweep end to end", async () => {
      getSiteConfigMock.mockResolvedValue({
        egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
      });
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^network$/i });

      testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms", via: "proxy" });
      await user.click(screen.getByRole("button", { name: /^test connectivity$/i }));
      await screen.findByText("Reached · via proxy");

      // Reached, but the configured redirect is untested — the gate holds
      // with its own head/sentence, and the footer's action is the fix.
      expect(screen.queryByRole("button", { name: /^next: review$/i })).not.toBeInTheDocument();
      expect(screen.getByText("Redirects aren't proven yet")).toBeInTheDocument();
      // #497: the same reason also renders a second time, as the rail's own
      // visible refusal text on every step this gate still blocks (Workspaces,
      // Review) — hence getAllByText, not getByText.
      expect(screen.getAllByText(/every configured redirect has to prove reached/i).length).toBeGreaterThan(0);

      // Clicking it dispatches into the step: switches to the egress tab and
      // fires every row's real probe — the full footer→step wiring, proven.
      testRedirectMock.mockResolvedValueOnce({ state: "reached", detail: "reachable via the mirror" });
      await user.click(screen.getByRole("button", { name: /^test the redirect$/i }));
      await screen.findByText("Reached");
      expect(await screen.findByRole("button", { name: /^next: review$/i })).toBeEnabled();
    });

    it("the gate survives leaving and re-entering the step — no re-test needed", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      // #213 — Secrets is off the required walk now: leave via the rail
      // (the gate being open makes it clickable), not a "Next:" click.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
      await screen.findByRole("heading", { name: /^secrets$/i });

      // Back to Corporate network via the rail: the step body remounted, but
      // the gate AND the sub-tab — held by the orchestrator, not the step —
      // still remember the no_runner pass and that we left from Egress
      // redirection.
      await user.click(within(nav).getByRole("button", { name: /^network/i }));
      await screen.findByRole("heading", { name: /^network$/i });
      expect(screen.getByText(/nothing redirected on this host/i)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^next: review$/i })).toBeEnabled();
      // The mirror again: Back inside the step returns to Host proxy, where
      // the remembered no_runner verdict is still on screen.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      expect(await screen.findByText("Can't test here")).toBeInTheDocument();
    });

    // steps.ts states the invariant outright ("There is no click-past"), so
    // it must be enforced on the always-visible rail too, not just the
    // footer's Next button — otherwise the rail lets the SAME jump happen in
    // one click with no friction.
    it("the rail can't click past a blocked gate — same rule the footer's Next enforces; backward jumps are unaffected", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^network$/i });
      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];

      // Untested (blocked): clicking Integrations in the rail is a no-op.
      await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
      expect(screen.getByRole("heading", { name: /^network$/i })).toBeInTheDocument();

      // Backward is never gated — only forward click-PAST is what the rule guards.
      await user.click(within(nav).getByRole("button", { name: /environment/i }));
      expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();

      // The SAME jump is blocked from an EARLIER step too — not just
      // while sitting on corp_network itself. A rail click straight from
      // Environment to Integrations (skipping over corp_network entirely,
      // never having visited it this render) must be a no-op just the same.
      await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
      expect(screen.getByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();

      // #497: anchored — Secrets' now-visible refusal reason text ("...
      // assumes the network works...") is part of its own accessible name
      // too, and would otherwise ambiguously match this unanchored regex.
      await user.click(within(nav).getByRole("button", { name: /^network/i }));
      await screen.findByRole("heading", { name: /^network$/i });

      // Proven: the identical rail click now works.
      await clearCorpNetworkGate();
      await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
      expect(await screen.findByRole("heading", { name: /^secrets$/i })).toBeInTheDocument();
    });
  });
});
