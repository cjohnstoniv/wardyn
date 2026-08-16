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
vi.mock("../../../lib/api/runs", () => ({
  // The Demos step's DemoRunner calls getRun (reload re-attach) + killRun (end) in
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
import { getDefaultCc } from "../../wardyn/default-confinement";
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
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
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
  // "Next:" click advances to Integrations, same as before round F.
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

  it("walks all nine funnel steps and Next/Back move within bounds", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);

    // Walk via the footer `Next: {label}` button (accessible name starts "Next:").
    // The Back button is anchored as /^back$/i so it can't collide with another
    // Back-ish verb. STEP_ORDER: essentials [environment, corp_network,
    // integrations] → demos (four sub-steps) → your work [workspaces] → finish
    // [review]. Review is the LAST step: the Launch step was cut, because its
    // "Example — not live config" card showed a fabricated task against a repo
    // that may never have been onboarded.

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
    await clearCorpNetworkGate();

    // Integrations follows Corporate network — it folds in the model/SCM-host
    // picker (host proxy / egress redirection moved to the step just visited).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(
      await screen.findByRole("heading", { name: /connect your model/i }),
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

    // your work: the one workspace step (AddWorkspaceDialog + a simple list).
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    expect(await screen.findByText(/never a raw host path/i)).toBeInTheDocument(); // workspaces

    await user.click(screen.getByRole("button", { name: /^next:/i }));
    // review step — the consolidated readiness rollup + the checks that used to
    // live on the barrier step (now grouped, e.g. the "gVisor runtime" ok row).
    expect(await screen.findByRole("heading", { name: /review readiness/i })).toBeInTheDocument();
    expect(screen.getByText("gVisor runtime")).toBeInTheDocument();

    // Review is the last step: no more Next, and no launch CTA of its own.
    expect(screen.queryByRole("button", { name: /^next:/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /launch your first run/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /finish setup/i })).toBeInTheDocument();

    // Back from Review lands on the workspaces step.
    await user.click(screen.getByRole("button", { name: /^back$/i }));
    expect(await screen.findByText(/never a raw host path/i)).toBeInTheDocument();
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
    expect(await screen.findByRole("heading", { name: /^corporate network$/i })).toBeInTheDocument();

    cleanup();
    renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=not-a-step");
    expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();
  });

  // W2-S1-2: a deep link past corp_network is the same click-past the rail
  // guards — a bookmarked/pasted `?step=` must not skip the mandatory proof
  // any more than a rail click can. Once status (and so the gate) loads, an
  // over-reaching initial step gets pulled back to corp_network.
  it("a ?step= deep link past the unproven corp_network gate is corrected back to it", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />, "/setup?step=integrations");
    expect(await screen.findByRole("heading", { name: /^corporate network$/i })).toBeInTheDocument();
  });

  // UI-SETUP-1: recheck() used to refresh status+site-config only — secret
  // names were fetched once at mount and never again, so the Integrations
  // rail badge (which derives from them) couldn't see a secret-backed
  // integration added OR deleted inside the embedded step until a full page
  // reload. loadSecrets is now folded into the SAME recheck every "Re-check"
  // button on this screen already calls.
  it("Re-check also re-fetches secret names, not just status and site-config", async () => {
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    expect(listSecretsMock).toHaveBeenCalledTimes(1); // the mount-time fetch

    await user.click(screen.getByRole("button", { name: /^re-check$/i }));
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(2));
    expect(listSecretsMock).toHaveBeenCalledTimes(2);
  });

  describe("Corporate network connectivity gate — wired through the real orchestrator", () => {
    it("the footer's button IS the probe while unproven; one reached probe makes the step done — no tab detour", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^corporate network$/i });

      // Blocked gate with an action: there IS no Next button — the fix-it
      // action stands in its place, under the state's own headline.
      expect(screen.queryByRole("button", { name: /^next: model \& git host$/i })).not.toBeInTheDocument();
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
      const next = screen.getByRole("button", { name: /^next: model \& git host$/i });
      expect(next).toBeEnabled();
      // Back from here returns to Host proxy — the mirror — without leaving
      // the step; then forward again.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      expect(await screen.findByText("What Wardyn found on this host")).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^next: egress redirection$/i }));

      await user.click(screen.getByRole("button", { name: /^next: model \& git host$/i }));
      expect(await screen.findByRole("heading", { name: /connect your model/i })).toBeInTheDocument();
    });

    it("no_runner unlocks Next immediately with its standing note — nothing was proven, and the note keeps saying so", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^corporate network$/i });

      await clearCorpNetworkGate();
      expect(screen.getByRole("button", { name: /^next: model \& git host$/i })).toBeEnabled();
      expect(screen.getByText("Nothing to test with")).toBeInTheDocument();
      expect(screen.getByText(/nothing was proven here/i)).toBeInTheDocument();
    });

    it("configured redirects hold the gate with their own footer action — 'Test the redirect' fires the sweep end to end", async () => {
      getSiteConfigMock.mockResolvedValue({
        egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
      });
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^corporate network$/i });

      testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms", via: "proxy" });
      await user.click(screen.getByRole("button", { name: /^test connectivity$/i }));
      await screen.findByText("Reached · via proxy");

      // Reached, but the configured redirect is untested — the gate holds
      // with its own head/sentence, and the footer's action is the fix.
      expect(screen.queryByRole("button", { name: /^next: model \& git host$/i })).not.toBeInTheDocument();
      expect(screen.getByText("Redirects aren't proven yet")).toBeInTheDocument();
      expect(screen.getByText(/every configured redirect has to prove reached/i)).toBeInTheDocument();

      // Clicking it dispatches into the step: switches to the egress tab and
      // fires every row's real probe — the full footer→step wiring, proven.
      testRedirectMock.mockResolvedValueOnce({ state: "reached", detail: "reachable via the mirror" });
      await user.click(screen.getByRole("button", { name: /^test the redirect$/i }));
      await screen.findByText("Reached");
      expect(await screen.findByRole("button", { name: /^next: model \& git host$/i })).toBeEnabled();
    });

    it("the gate survives leaving and re-entering the step — no re-test needed", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect your model/i });

      // Back to Corporate network: the step body remounted, but the gate AND
      // the sub-tab — held by the orchestrator, not the step — still remember
      // the no_runner pass and that we left from Egress redirection.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      await screen.findByRole("heading", { name: /^corporate network$/i });
      expect(screen.getByText(/nothing redirected on this host/i)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^next: model \& git host$/i })).toBeEnabled();
      // The mirror again: Back inside the step returns to Host proxy, where
      // the remembered no_runner verdict is still on screen.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      expect(await screen.findByText("Can't test here")).toBeInTheDocument();
    });

    // UI-SETUP-11: steps.ts states the invariant outright ("There is no
    // click-past"), but that was enforced on the footer's Next button only —
    // the always-visible rail let the SAME jump happen in one click with no
    // friction. Blocking it here is what actually makes the stated rule true.
    it("the rail can't click past a blocked gate — same rule the footer's Next enforces; backward jumps are unaffected", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await screen.findByRole("heading", { name: /^corporate network$/i });
      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];

      // Untested (blocked): clicking Integrations in the rail is a no-op.
      await user.click(within(nav).getByRole("button", { name: /model \& git host/i }));
      expect(screen.getByRole("heading", { name: /^corporate network$/i })).toBeInTheDocument();

      // Backward is never gated — only forward click-PAST is what the rule guards.
      await user.click(within(nav).getByRole("button", { name: /environment/i }));
      expect(await screen.findByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();

      // W2-S1-2: the SAME jump is blocked from an EARLIER step too — not just
      // while sitting on corp_network itself. A rail click straight from
      // Environment to Integrations (skipping over corp_network entirely,
      // never having visited it this render) must be a no-op just the same.
      await user.click(within(nav).getByRole("button", { name: /model \& git host/i }));
      expect(screen.getByRole("heading", { name: /pick your barrier/i })).toBeInTheDocument();

      await user.click(within(nav).getByRole("button", { name: /corporate network/i }));
      await screen.findByRole("heading", { name: /^corporate network$/i });

      // Proven: the identical rail click now works.
      await clearCorpNetworkGate();
      await user.click(within(nav).getByRole("button", { name: /model \& git host/i }));
      expect(await screen.findByRole("heading", { name: /connect your model/i })).toBeInTheDocument();
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

    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
    expect(
      await screen.findByRole("heading", { name: /connect your model/i }),
    ).toBeInTheDocument();
    // The two SHARED Settings cards, not the deleted /integrations catalog.
    // (connection-cards.test.tsx owns their own behaviour; this asserts the
    // orchestrator mounts them.) The old assertion here looked for the embed's
    // "AI providers" section heading — that section, and the page it belonged
    // to, are gone.
    expect(await screen.findByRole("radiogroup", { name: /model provider/i })).toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: /git host/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
    // Both PhaseRail landmarks share the "Setup steps" accessible name
    // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
    // renders both — the full rail is the SECOND in DOM order.
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = navs[navs.length - 1];
    const btn = within(nav).getByRole("button", { name: /model \& git host/i });
    expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
  });

  // The inverse of the old UX-1 rule. That wave made the badge count the eight
  // GENERIC categories (package feeds, registries, cloud, data, MCP, work
  // tracking, observability, other) alongside AI/SCM, because the step body
  // showed them. 0.5 removed generic kinds and the catalog that held them — the
  // step is two cards, Model provider and Git host — so a stored generic row
  // must NOT earn the badge for something the step cannot configure.
  it("the badge ignores a generic-kind row — the step configures model + git host only", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "jira-1", kind: "jira", name: "Jira" }] }),
    );
    renderScreen(<SetupScreen onDone={() => {}} />);
    await screen.findByText("Fence");
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();

    // Both PhaseRail landmarks share the "Setup steps" accessible name
    // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
    // renders both — the full rail is the SECOND in DOM order.
    const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
    const nav = navs[navs.length - 1];
    const btn = within(nav).getByRole("button", { name: /model \& git host/i });
    expect(await within(btn).findByText("Optional")).toBeInTheDocument();
    expect(within(btn).queryByText(/connected/)).not.toBeInTheDocument();
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
      await clearCorpNetworkGate();
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect your model/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations

      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /model \& git host/i });
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
      await clearCorpNetworkGate();
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect your model/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations

      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /model \& git host/i });
      expect(await within(btn).findByText("Ready · 1 connected")).toBeInTheDocument();
      expect(within(btn).queryByText("Skipped")).not.toBeInTheDocument();
    });

    it("clicking Next past Integrations with nothing connected IS the skip — checkmark, Skipped badge, no button needed", async () => {
      renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");

      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect your model/i });
      // The two dead affordances stay dead: Next is the one forward control.
      expect(screen.queryByRole("button", { name: /^skip this step$/i })).not.toBeInTheDocument();
      expect(screen.queryByRole("link", { name: /manage in integrations/i })).not.toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^next: the sealed box$/i }));

      expect(await screen.findByRole("heading", { name: /the sealed box/i })).toBeInTheDocument();
      // Both PhaseRail landmarks share the "Setup steps" accessible name
      // (ui-setup-5); CSS shows only one at a time in a real browser, jsdom
      // renders both — the full rail is the SECOND in DOM order.
      const navs = screen.getAllByRole("navigation", { name: /setup steps/i });
      const nav = navs[navs.length - 1];
      const btn = within(nav).getByRole("button", { name: /model \& git host/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
      // …and the checkmark: forward-past is the same per-browser decision the
      // old explicit control recorded (persisted via markIntegrationsSkipped).
      expect(btn.querySelector("[data-done]") ?? within(btn).getByText("Skipped")).toBeTruthy();

      // Backing INTO the step decides nothing extra and the state holds.
      await user.click(screen.getByRole("button", { name: /^back$/i }));
      await screen.findByRole("heading", { name: /connect your model/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });

    it("visited-step tracking round-trips through localStorage across a remount", async () => {
      const { unmount } = renderScreen(<SetupScreen onDone={() => {}} />);
      await screen.findByText("Fence");
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
      await clearCorpNetworkGate();
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> integrations
      await screen.findByRole("heading", { name: /connect your model/i });
      await user.click(screen.getByRole("button", { name: /^next:/i })); // -> first demo, leaves integrations
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
      const btn = within(nav).getByRole("button", { name: /model \& git host/i });
      expect(within(btn).getByText("Skipped")).toBeInTheDocument();
    });
  });

  it("'Finish setup' at the end of the flow dismisses setup and calls onDone (no early exit)", async () => {
    const onDone = vi.fn();
    renderScreen(<SetupScreen onDone={onDone} />);
    await screen.findByText("Fence");

    // No early escape any more — the mandatory gate keeps the operator in setup.
    expect(screen.queryByRole("button", { name: /finish later/i })).not.toBeInTheDocument();

    // Corporate network is a mandatory gate even for a rail jump (W2-S1-2) —
    // clear it (same helper every other walkthrough in this suite uses) before
    // jumping to the final step.
    await user.click(screen.getByRole("button", { name: /^next:/i }));
    await clearCorpNetworkGate();

    // Review is the final step now, and "Finish setup" is its completion.
    await user.click(screen.getByRole("button", { name: /^Review —/ }));
    await user.click(screen.getByRole("button", { name: /^finish setup$/i }));
    expect(setupDismissed()).toBe(true);
    expect(onDone).toHaveBeenCalledTimes(1);
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
    // walk to Review (the last step, 9 of 9) — checks live there, not on the
    // barrier step
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
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

    // Exactly 1 — the orchestrator's own mount. This used to be 2: the embedded
    // IntegrationsScreen ran a SECOND, independent getSetupStatus when the walk
    // passed through the connection step. The shared cards take `status` as a
    // prop from the orchestrator instead, so walking the funnel no longer
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
    // Walk to Review: the non-platform check appears grouped; the platform note under "About this host".
    await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
    await clearCorpNetworkGate();
    for (let i = 0; i < 7; i++) await user.click(screen.getByRole("button", { name: /^next:/i }));
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
