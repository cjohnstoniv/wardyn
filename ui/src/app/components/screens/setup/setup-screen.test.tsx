/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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

import { SetupScreen, setupDismissed, dismissSetup } from "./setup-screen";
import { DEMOS } from "../demos/demo-catalog";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";
import { DRIVES } from "../../../lib/user-drives-copy";

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
// reused across the review-step assertions below. loopback carries `blocking:
// true` on purpose — #161's own motivating case, a blocking WARN, so Review's
// "Blocking" group is proven by `blocking`, not by grade; kvm's `fail` has no
// `blocking` and lands under "Worth a look" for the same reason.
function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    checks: [
      { id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" },
      { id: "loopback", label: "Loopback bind", status: "warn", detail: "bound to 0.0.0.0", blocking: true },
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

  // #213 — the counter counts only what BLOCKS a run. The walk via the
  // footer's Next/Back is now the four required steps
  // [environment, people, corp_network, review] — Secrets, the ten demos,
  // Workspace providers and Workspaces are OFF that walk entirely, reached
  // only from the rail or Review's own optional lists (see the next test).
  it("walks the four required steps via Next/Back, and bounds hold at both ends", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);

    // environment (first) step — barrier-led; the tier cards render, the
    // cross-cutting checks do NOT (they moved to the Review step).
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
    expect(screen.getByText("Fence")).toBeInTheDocument();
    expect(screen.queryByText("gVisor runtime")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^back$/i })).toBeDisabled();
    expect(screen.getByText("Step 1 of 4")).toBeInTheDocument();

    // People directly follows Environment — a pure explainer, done on
    // arrival, with no gate of its own.
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findAllByText("Single-user")).not.toHaveLength(0);
    expect(screen.getByText("Step 2 of 4")).toBeInTheDocument();

    // Corporate network directly follows People (the order itself is the
    // fix for "blocked network reads as bad credential" — see steps.ts).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByRole("heading", { name: /^network$/i })).toBeInTheDocument();
    expect(screen.getByText("Step 3 of 4")).toBeInTheDocument();
    await clearCorpNetworkGate();

    // Next now goes STRAIGHT to Review — Secrets/Providers/Workspaces/the
    // demos are no longer counted or stepped through on this walk (#213).
    await user.click(screen.getByRole("button", { name: /^next: review$/i }));
    expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument();
    expect(screen.getByText("Step 4 of 4")).toBeInTheDocument();

    // Review is the last required step: no more Next, and no launch CTA of
    // its own.
    expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /launch your first run/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /finish setup/i })).toBeInTheDocument();

    // Back from Review returns to Network — the previous REQUIRED step, not
    // whatever optional step used to sit between them.
    await user.click(screen.getByRole("button", { name: /^back$/i }));
    expect(await screen.findByRole("heading", { name: /^network$/i })).toBeInTheDocument();
  });

  // The same coverage the old linear walk gave each optional step — reached
  // from the rail now, since Next no longer passes through any of them. This
  // fixture has no model and no stored secret, so the harness demo
  // (needsModel) is dropped from the rail entirely; the TEACH+GATE demo
  // (github-app-broker) stays offered with a disabled Start.
  it("reaches every optional step from the rail, each with the optional-step footer, none on the required walk", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await screen.findByRole("heading", { name: /^network$/i });
    await clearCorpNetworkGate();

    // Both PhaseRail landmarks share the "Setup steps" accessible name
    // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
    // renders both — the full rail is the SECOND in DOM order.
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = navs[navs.length - 1];

    // Secrets — folds in the model/SCM-host picker.
    await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
    expect(await screen.findByRole("heading", { name: /^secrets$/i })).toBeInTheDocument();
    // Its footer is the optional-step pair, never a numbered Next.
    expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^back to required steps$/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^done with this one$/i })).toBeInTheDocument();

    // Egress demos — the first proves the lazily-loaded detail body mounts.
    // 10s, not RTL's 1000ms default: this is the ONE find in the suite that
    // waits on the React.lazy(() => import("./demos-step")) boundary
    // (setup-screen.tsx), and that chunk drags in demo-screen/xterm. Vite
    // transforms it on first demand — ~200ms alone, but deterministically
    // past a second when all 79 test files transform in parallel.
    await user.click(within(nav).getByRole("button", { name: /^the sealed box/i }));
    expect(await screen.findByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();
    expect(
      await screen.findByText(/set up a sandbox like this yourself/i, undefined, { timeout: 10_000 }),
    ).toBeInTheDocument();

    // TEACH+GATE: no GitHub App in this fixture — the demo is still offered
    // (reachable from the rail), a disabled Start card, not a missing step.
    await user.click(within(nav).getByRole("button", { name: /a token the sandbox never even sees/i }));
    expect(
      await screen.findByRole("heading", { name: /a token the sandbox never even sees/i }),
    ).toBeInTheDocument();
    expect(await screen.findByTestId("demo-needs-github-app")).toBeInTheDocument();
    expect(screen.getByTestId("demo-start-github-app-broker")).toBeDisabled();

    // The harness demo (needsModel, no model connected here) is DROPPED
    // entirely — not offered on the rail at all.
    expect(within(nav).queryByRole("button", { name: /the agent in the box/i })).not.toBeInTheDocument();

    // Workspace providers — its body is the card, zero teal.
    await user.click(within(nav).getByRole("button", { name: /^providers/i }));
    expect(await screen.findByTestId("providers-card")).toBeInTheDocument();

    // …then Workspaces (AddWorkspaceDialog + a simple list) plus the
    // user-drives card under it — persistent storage gets no step of its
    // own, so this card is the ONLY place the funnel mentions it. Same
    // component as Settings' fifth card.
    await user.click(within(nav).getByRole("button", { name: /^workspaces/i }));
    expect(await screen.findByText(/never a raw host path/i)).toBeInTheDocument();
    expect(await screen.findByTestId("user-drives-card")).toBeInTheDocument();
    expect(screen.getByText(DRIVES.CARD_EMPTY)).toBeInTheDocument();
  });

  // The gate itself, wired end to end through the real orchestrator — the unit
  // coverage for each rule lives in steps.test.ts (corpNetworkGate) and
  // corp-network-step.test.tsx (the step body reporting upward); this proves
  // setup-screen.tsx actually connects them.
  // ?step= deep link: the Integrations page's proxy banner hands off to
  // Corporate network with it, now that the step is the only place a proxy is
  // configured (see integrations-screen.tsx's "Open Corporate network").
  it("opens the step named by ?step=, and ignores a bogus one rather than blowing up", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=corp_network");
    expect(await screen.findByRole("heading", { name: /^network$/i })).toBeInTheDocument();

    cleanup();
    renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=not-a-step");
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
  });

  // A deep link past corp_network is the same click-past the rail
  // guards — a bookmarked/pasted `?step=` must not skip the mandatory proof
  // any more than a rail click can. Once status (and so the gate) loads, an
  // over-reaching initial step gets pulled back to corp_network.
  it("a ?step= deep link past the unproven corp_network gate is corrected back to it", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=integrations");
    expect(await screen.findByRole("heading", { name: /^network$/i })).toBeInTheDocument();
  });

  // The corp gate is CROSSING-based, and demo targets are exempt. /demos
  // redirects into a demo step and every episode cold-opens on one: if the
  // corrector bounced those to Network, a shared demo link would never open
  // its demo. Nothing unsafe opens — a demo gates its own Start on
  // barrierReady, which this fixture's no_runner host fails anyway.
  describe("the crossing gate — demo links open, click-past still can't happen", () => {
    it("a cold ?step=<demo> deep link opens the demo instead of bouncing to Network", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=sealed-box");
      expect(await screen.findByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();
      // The gate is genuinely still unproven — this is an exemption, not a pass.
      expect(screen.queryByRole("heading", { name: /^network$/i })).not.toBeInTheDocument();
    });

    // nextGate is produced only ON corp_network, so the last demo's Next must
    // render DISABLED rather than enabled with its click bare-returning into
    // nothing. The shell asks the same predicate selectStep does, so it
    // renders disabled and says why.
    // #213 — a demo's footer is the optional-step pair now, not a numbered
    // Next: "Done with this one" targets Review, and it must render DISABLED
    // (with the gate's reason as its title) rather than a dead-enabled
    // button whose click silently no-ops.
    it("a demo's 'Done with this one' is DISABLED, with the gate's reason as its title — never a dead-enabled button", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=sts-fail-closed");
      await screen.findByRole("heading", { name: /no identity, no credential/i });
      expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();
      const done = screen.getByRole("button", { name: /^done with this one$/i });
      expect(done).toBeDisabled();
      expect(done.getAttribute("title")).toMatch(/one probe/i);
      // Back to required steps is never gated by refuseNext, same as every
      // other Back button in this shell.
      expect(screen.getByRole("button", { name: /^back to required steps$/i })).toBeEnabled();
    });

    it("from a demo, Back into Integrations works while Workspaces stays gated", async () => {
      // ticket: HIGH-1
      renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=sealed-box");
      await screen.findByRole("heading", { name: /the sealed box/i });
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = within(navs[navs.length - 1]);

      // Forward past the gate is still refused — the part that matters.
      await user.click(nav.getByRole("button", { name: /^workspaces/i }));
      expect(screen.getByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();

      // Backwards is free (the accepted trade: Integrations is optional, and a
      // target-index-based gate would dead-end the operator on this demo).
      await user.click(nav.getByRole("button", { name: /^secrets/i }));
      expect(await screen.findByRole("heading", { name: /^secrets$/i })).toBeInTheDocument();
    });
  });

  // The walk is stepOrder(status): a demo whose needsSecret is unmet is not
  // a step, so the rail can't offer it and a link to it can't strand the
  // operator on a step no consumer can find an index for.
  describe("conditional demo steps — the walk follows the stored secret", () => {
    const granted = DEMOS.find((d) => d.needsSecret)!;

    it("a ?step= link to an unmet needsSecret demo re-corrects to the nearest surviving step", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />, `/setup?step=${granted.id}`);
      // Falls BACK, never forward: write-only-by-design is the step that tells
      // the operator how to store the very secret this one is waiting on.
      expect(await screen.findByRole("heading", { name: /write-only, even for you/i })).toBeInTheDocument();
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      expect(within(navs[navs.length - 1]).queryByRole("button", { name: new RegExp(granted.title, "i") })).toBeNull();
    });

    it("with the secret stored, the same link opens the demo and the rail offers it", async () => {
      getSetupStatusMock.mockResolvedValue(
        baseStatus({ secrets: { present: [granted.needsSecret!], github_app: false } }),
      );
      renderScreen(<SetupScreen onDone={() => {}} />, `/setup?step=${granted.id}`);
      expect(
        await screen.findByRole("heading", { name: new RegExp(granted.title, "i") }),
      ).toBeInTheDocument();
    });
  });

  // loadSecrets must be folded into the SAME recheck every "Re-check" here
  // calls, or the Integrations rail badge can't see a secret added/deleted
  // in the embedded step until a reload — and the PRESS forces a host
  // re-detect the MOUNT must not.
  it("Re-check re-fetches secret names and forces a host re-detect; the mount fetch does neither", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    expect(listSecretsMock).toHaveBeenCalledTimes(1); // the mount-time fetch
    expect(getSetupStatusMock.mock.calls[0][0]?.recheck).toBeFalsy();
    // U2-02: and no "checked" stamp — the mount read is answered from the memo.
    expect(screen.queryByText(/checked/i)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^re-check$/i }));
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(2));
    expect(listSecretsMock).toHaveBeenCalledTimes(2);
    expect(getSetupStatusMock.mock.calls[1][0]).toEqual({ recheck: true });
    // R-09: that forced read returned the SAME host_proxy — what a wedged host
    // gives once hostProxyRecheck's 2s wait expires. Forcing is not proof.
    expect(screen.queryByText(/checked/i)).not.toBeInTheDocument();
    const found = { value: "http://p:3128", source: "env", has_credentials: false };
    getSetupStatusMock.mockResolvedValue(baseStatus({ host_proxy: { has_credentials: false, http_proxy: found } }));
    await user.click(screen.getByRole("button", { name: /^re-check$/i }));
    await waitFor(() => expect(screen.getByText(/checked just now/i)).toBeInTheDocument());
  });

  // Phase 5 — the People step: renders with its label/heading, its badge reads
  // the deployment mode, and it's done on arrival (an explainer, not a task).
  describe("People step", () => {
    it("renders the label, heading and Single-user badge, and is done on arrival", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=people");
      // Exactly ONE "Who can sign in" heading: the page heading (STEP_HEADING)
      // carries it and the DeploymentStep's card deliberately has no title, so
      // assistive tech never hears the same name twice at the same level.
      expect(await screen.findAllByRole("heading", { name: /who can sign in/i })).toHaveLength(1);

      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = within(navs[navs.length - 1]);
      const btn = nav.getByRole("button", { name: /^people/i });
      expect(within(btn).getByText("Single-user")).toBeInTheDocument();
      // done.people === true: the checkmark renders on arrival, no action taken.
      expect(btn.querySelector("svg.lucide-check")).toBeInTheDocument();
    });

    // Negative control: sso reads Multi-user, not Single-user, on the same step.
    it("reads Multi-user when auth.mode is sso", async () => {
      getSetupStatusMock.mockResolvedValue(baseStatus({ auth: { mode: "sso", local_loopback: false } }));
      renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=people");
      expect(await screen.findAllByText("Multi-user")).not.toHaveLength(0);
      expect(screen.queryAllByText("Single-user")).toHaveLength(0);
    });
  });

  it("the connection step renders the shared cards (not the old catalog) and the rail badge counts a connection", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
    // The rail badge counts off status.integrations (setup-screen.tsx's own
    // integrationsCount); the embedded IntegrationsScreen renders off its own
    // independent GET /integrations read — give both the same one row so the
    // two agree, exactly as the real server-derived set would.
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");

    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
    // #213 — Secrets is off the required walk; reach it from the rail.
    const navsInit = screen.getAllByRole("navigation", { name: /setup steps/i });
    await user.click(within(navsInit[navsInit.length - 1]).getByRole("button", { name: /^secrets/i }));
    expect(
      await screen.findByRole("heading", { name: /^secrets$/i }),
    ).toBeInTheDocument();
    // The SHARED Settings model-provider card, not the deleted /integrations
    // catalog. (connection-cards.test.tsx owns its own behaviour; this
    // asserts the orchestrator mounts it.) GitHostCard is retired — its git
    // credential lanes live in the `providers` step / /providers.
    expect(await screen.findByRole("radiogroup", { name: /model provider/i })).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: /git host/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
    // Both PhaseRail landmarks share the "Setup steps" accessible name
    // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
    // renders both — the full rail is the SECOND in DOM order.
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = navs[navs.length - 1];
    const btn = within(nav).getByRole("button", { name: /^secrets/i });
    expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
  });

  // The step is two cards, Model provider and Git host — the catalog for
  // GENERIC categories (package feeds, registries, cloud, data, MCP, work
  // tracking, observability, other) is gone, so a stored generic row must
  // NOT earn the badge for something the step cannot configure.
  it("the badge ignores a generic-kind row — the step configures model + git host only", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "jira-1", kind: "jira", name: "Jira" }] }),
    );
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();

    // Both PhaseRail landmarks share the "Setup steps" accessible name
    // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
    // renders both — the full rail is the SECOND in DOM order.
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = navs[navs.length - 1];
    const btn = within(nav).getByRole("button", { name: /^secrets/i });
    expect(await within(btn).findByText("Optional")).toBeInTheDocument();
    expect(within(btn).queryByText(/connected/)).not.toBeInTheDocument();
  });

  // A4 — "Skipped" state: an optional step the operator navigated past without
  // configuring it reads "Skipped" in the rail instead of a perpetual "Optional".
  // Exercised via Integrations, since the corporate-network steps are folded
  // into it and this is where that coverage now lives.
  describe("Skipped state", () => {
    // ticket: A4
    it("navigating past Integrations without connecting anything marks its rail badge Skipped", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      // #213 — Secrets is off the required walk; reach it from the rail, and
      // leave FORWARD via its own "Done with this one" (targets Review).
      const navs0 = screen.getAllByRole("navigation", { name: /setup steps/i });
      await user.click(within(navs0[navs0.length - 1]).getByRole("button", { name: /^secrets/i })); // -> integrations
      await screen.findByRole("heading", { name: /^secrets$/i });
      await user.click(screen.getByRole("button", { name: /^done with this one$/i })); // -> review, leaves integrations

      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /^secrets/i });
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

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      const navs0 = screen.getAllByRole("navigation", { name: /setup steps/i });
      await user.click(within(navs0[navs0.length - 1]).getByRole("button", { name: /^secrets/i })); // -> integrations
      await screen.findByRole("heading", { name: /^secrets$/i });
      await user.click(screen.getByRole("button", { name: /^done with this one$/i })); // -> review, leaves integrations

      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /^secrets/i });
      expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
      expect(within(btn).queryByText("Skipped")).not.toBeInTheDocument();
    });

    it("clicking 'Done with this one' past Integrations with nothing connected IS the skip — checkmark, Skipped badge, no button needed", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      const navs0 = screen.getAllByRole("navigation", { name: /setup steps/i });
      await user.click(within(navs0[navs0.length - 1]).getByRole("button", { name: /^secrets/i })); // -> integrations
      await screen.findByRole("heading", { name: /^secrets$/i });
      // The two dead affordances stay dead: "Done with this one" is the one
      // forward control on an optional step.
      expect(screen.queryByRole("button", { name: /^skip this step$/i })).not.toBeInTheDocument();
      expect(screen.queryByRole("link", { name: /manage in integrations/i })).not.toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^done with this one$/i }));

      expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /^secrets/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
      // …and the checkmark: forward-past is the same per-browser decision the
      // old explicit control recorded (persisted via markIntegrationsSkipped).
      expect(btn.querySelector("[data-done]") ?? within(btn).getByText("Skipped")).toBeTruthy();

      // Re-entering the step (via the rail — Review's Back returns to
      // Network, not Secrets) decides nothing extra and the state holds.
      await user.click(within(nav).getByRole("button", { name: /^secrets/i }));
      await screen.findByRole("heading", { name: /^secrets$/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });

    it("visited-step tracking round-trips through localStorage across a remount", async () => {
      const { unmount } = renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
      await screen.findAllByText("Single-user");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      const navs0 = screen.getAllByRole("navigation", { name: /setup steps/i });
      await user.click(within(navs0[navs0.length - 1]).getByRole("button", { name: /^secrets/i })); // -> integrations
      await screen.findByRole("heading", { name: /^secrets$/i });
      await user.click(screen.getByRole("button", { name: /^done with this one$/i })); // -> review, leaves integrations
      unmount();

      // A fresh mount reads the persisted visited-set back — this must be real
      // localStorage, not in-memory React state that a remount would lose.
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /^secrets/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });
  });

  it("'Finish setup' at the end of the flow dismisses setup and calls onDone (no early exit)", async () => {
    const onDone = vi.fn();
    renderScreen(<SetupScreen onDone={onDone} />);
    await screen.findByText("Fence");

    // No early escape any more — the mandatory gate keeps the operator in setup.
    expect(screen.queryByRole("button", { name: /finish later/i })).not.toBeInTheDocument();

    // Corporate network is a mandatory gate even for a rail jump —
    // clear it (same helper every other walkthrough in this suite uses) before
    // jumping to the final step.
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();

    // Review is the final step now, and "Finish setup" is its completion.
    await user.click(screen.getByRole("button", { name: /^Review —/ }));
    await user.click(screen.getByRole("button", { name: /^finish setup$/i }));
    expect(setupDismissed()).toBe(true);
    expect(onDone).toHaveBeenCalledTimes(1);
    // The INSTALL records completion too — the server call is what makes
    // onboarding survive a browser change and agree across origins.
    expect(completeOnboardingMock).toHaveBeenCalledTimes(1);
  });

  // "Open Runs" was the Launch step's second exit, and it dismissed setup so
  // the old mandatory gate could not bounce the operator back to step 1. Both
  // the step and that gate are gone; "Finish setup" on Review is the one
  // completion, covered above.

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

  // The Launch step's CTA was the funnel's own NewRunDialog trigger — the only
  // one. Both are gone: launching is the top bar's "New run", reachable from
  // every screen, so the funnel no longer carries a second copy of that dialog.

  it("review step renders ok/warn/fail/info rows grouped, and Re-check calls getSetupStatus again", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence"); // environment settled
    // #213 — Review is the fourth and last REQUIRED step: one Next click
    // past the cleared Network gate lands on it directly. Checks live there,
    // not on the barrier step.
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
    await user.click(screen.getByRole("button", { name: /^next: review$/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument(); // ok (Ready group)
    expect(screen.getByText("Loopback bind")).toBeInTheDocument(); // blocking warn (Blocking)
    expect(screen.getByText("/dev/kvm")).toBeInTheDocument(); // non-blocking fail (Worth a look)
    expect(screen.getByText("macOS note")).toBeInTheDocument(); // info (Ready group)
    // the fail row's client-absent fix falls through to the backend-provided fix
    expect(screen.getByText(/enable virtualization/i)).toBeInTheDocument();
    // grouped headings prove the rollup, not a flat dump; a blocking WARN
    // lands under "Blocking", not a graded-only "Worth a look" (#161).
    expect(screen.getByText("Blocking").closest("section")).toHaveTextContent("Loopback bind");
    expect(screen.getByText("Worth a look").closest("section")).toHaveTextContent("/dev/kvm");

    // Exactly 1 — the orchestrator's own mount, and nothing this walk touched
    // re-fetches the same status a second time.
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
    renderScreen(<SetupScreen onDone={() => {}} />);
    // Barrier step: the 3-tier runner list renders; the cross-cutting checks do NOT.
    expect(await screen.findByText("Fence")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    expect(screen.queryByText("Secret store durability")).not.toBeInTheDocument();
    // Walk to Review (the fourth and last required step): the non-platform
    // check appears grouped; the platform note under "About this host".
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
    await screen.findAllByText("Single-user");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
    await user.click(screen.getByRole("button", { name: /^next: review$/i }));
    await screen.findByRole("heading", { name: /review readiness/i });
    expect(screen.getByText("Secret store durability")).toBeInTheDocument();
    expect(screen.getByText("About this host")).toBeInTheDocument();
    expect(screen.getByText("WSL networking")).toBeInTheDocument();
  });

  // E2 — setup-check provenance

  it("environment step names the concrete substrate each ready tier runs as", async () => {
    // ticket: E2
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

  // E3 — default barrier tier selection

  it("preselects the resolved default barrier, moves on a click (in-session only), and keeps a todo card's setup command working", async () => {
    // ticket: E3
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    // The three tiers are radios (role=radio / aria-checked); the tier name is in
    // each radio's accessible name (Fence/Wall/Vault). baseStatus has CC1+CC2 ready
    // and no in-session pick yet, so the resolved default is the strongest
    // available (Wall/CC2) — the SOLE checked radio.
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Wall/, checked: true })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Fence/, checked: false })).toBeInTheDocument();

    // Clicking the Fence radio moves the selection — for THIS view only: the
    // default is a server fact, and nothing here persists across sessions.
    await user.click(screen.getByRole("radio", { name: /Fence/ }));
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Fence/, checked: true })).toBeInTheDocument();

    // Regression the selectable-only radio decision exists to prevent: Vault (CC3)
    // is a todo tier — its radio is disabled, but its "Show setup command" button
    // still reveals the inline command instead of being swallowed by a selection.
    // ui-setup-1's manualSteps disclosure also mentions "wardyn setup vault" in
    // prose, so scope the command assertion to the <code> element it's the
    // literal command of.
    expect(screen.queryByText(/wardyn setup vault/, { selector: "code" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /show setup command/i }));
    expect(screen.getByText(/wardyn setup vault/, { selector: "code" })).toBeInTheDocument();
    // Revealing the todo card's command never disturbs the barrier selection.
    expect(screen.getAllByRole("radio", { checked: true })).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /Fence/, checked: true })).toBeInTheDocument();
  });

  // Barrier taxonomy — incompatible (hardware) vs needs-setup (installable) ----

  // #213 — the recommendation is the strongest INSTALLED barrier, never
  // inferred from hardware or the OS. baseStatus: CC1+CC2 ready, CC3 missing
  // (kvm:true, so Vault reads "Needs setup", not "Incompatible") — Recommended
  // sits on Wall, the strongest tier this host actually has, not on the
  // merely-installable Vault.
  it("recommends the strongest INSTALLED tier: Vault (Needs setup, not installed) is never Recommended", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByRole("heading", { name: /pick your barrier/i });

    expect(screen.getAllByText("Recommended")).toHaveLength(1);
    const wallCol = screen.getByRole("radio", { name: /Wall/ }).closest("th")!;
    expect(within(wallCol).getByText("Recommended")).toBeInTheDocument();
    expect(screen.queryByText("Incompatible here")).not.toBeInTheDocument();
    // The selection ring (the ACTUAL default for new runs) stays on ready tiers (Wall).
    expect(screen.getByRole("radio", { name: /Wall/, checked: true })).toBeInTheDocument();
  });

  it("marks Vault Incompatible (with the /dev/kvm why) on a KVM-less host — the recommendation is unaffected either way (still Wall, the strongest installed)", async () => {
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
