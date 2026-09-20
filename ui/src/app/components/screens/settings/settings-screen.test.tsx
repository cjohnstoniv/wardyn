/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings — the card list itself, not the cards (each has its own suite).
// Three things are pinned here because they are DECISIONS, not rendering:
//
//   1. the five cards render in the order the page declares them,
//   2. Drives is the FIFTH — the LAST one (user-drives-prompt.md §6). It is the
//      only card whose position was ambiguous, because it landed fourth while
//      the spec called it a fifth card, so it is pinned by POSITION rather than
//      by presence alone,
//   3. it is SUPER-only: a security admin sees no card and Settings asks GET
//      /drives nothing. The card's own gate is pinned in user-drives-card
//      .test.tsx; this pins that Settings actually hands it a real provider
//      instead of coasting on the context's fail-open default.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

const getSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: { getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a) },
}));

const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({
  drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) },
}));

// The Providers card (replacing Git host, 0.7.2) summarises GET
// /workspace-providers the same way UserDrivesCard summarises GET /drives.
const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));

vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: () => Promise.resolve([]) },
}));

vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: vi.fn(), deleteSecret: vi.fn() },
}));

// The model card's sign-in pane drives a real PTY through xterm, which does not
// render in jsdom.
vi.mock("./harness-login-pane", () => ({
  HarnessLoginPane: () => <div data-testid="login-pane" />,
}));

import { SettingsScreen } from "./settings-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { DRIVES } from "../../../lib/user-drives-copy";
import { OperatorProvider } from "../../wardyn/operator-context";

function renderScreen(operator = true, operatorResolved = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} operatorResolved={operatorResolved} securityOperator>
        <SettingsScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  getSiteConfigMock.mockReset().mockResolvedValue({});
  getDrivesMock
    .mockReset()
    .mockResolvedValue({
      drives: [],
      grants: [],
      host_roots_configured: false,
      runner_target: "docker",
    });
  getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
});

describe("SettingsScreen", () => {
  // 0.7.2: Git host retired — Providers (setup/providers-card.tsx, the SAME
  // component the funnel's `providers` step renders) takes its THIRD-card
  // position (workspace-providers-prompt.md §6).
  it("draws Host, Model provider, Providers, SSH keys, Drives — in that order", async () => {
    renderScreen();
    await screen.findByTestId("user-drives-card");
    const html = document.body.innerHTML;
    const positions = ["Host", "Model provider", "Workspace providers", "Your SSH keys", "User drives"].map((label) =>
      html.indexOf(`>${label}<`),
    );
    for (const p of positions) expect(p).toBeGreaterThan(-1);
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
  });

  it("draws the drives card LAST — the fifth card §6 names, not the fourth", async () => {
    renderScreen();
    const card = await screen.findByTestId("user-drives-card");
    expect(card).toBeInTheDocument();
    // Position, not presence: it is the last thing in the card column.
    expect(card.parentElement?.lastElementChild).toBe(card);
    expect(card.parentElement?.children).toHaveLength(5);
    // …and it is the real card, summarising the same GET /drives the screen owns.
    expect(await screen.findByText(DRIVES.CARD_EMPTY)).toBeInTheDocument();
  });

  it("is SUPER-only: a security admin gets no card, and Settings asks /drives nothing", async () => {
    renderScreen(false);
    // The host card is the settle anchor — once it is on screen the ready
    // branch has rendered, so the absence below is a decision, not a race.
    expect(
      await screen.findByRole("heading", { name: "Host", level: 3 }),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("user-drives-card")).toBeNull();
    expect(getDrivesMock).not.toHaveBeenCalled();
  });
});

// setup-screen.tsx's sibling guard (member-cold-load lane, plan P3): GET
// /site-config is operatorOnly, and during the cold-load window `operator`
// reads the fail-open default `true`, so `operatorResolved` is the half that
// closes it — the mount `Promise.all` must not fire the read at ALL for a
// member, not just swallow its 403 into `null`.
describe("SettingsScreen — operatorResolved && operator guards the site-config read", () => {
  it("a member's cold /setup load fires no admin-only reads", async () => {
    renderScreen(/* operator */ false);
    expect(
      await screen.findByRole("heading", { name: "Host", level: 3 }),
    ).toBeInTheDocument();
    expect(getSiteConfigMock).not.toHaveBeenCalled();
  });

  it("an admin's cold /setup load still fetches it", async () => {
    renderScreen(/* operator */ true);
    await screen.findByRole("heading", { name: "Host", level: 3 });
    expect(getSiteConfigMock).toHaveBeenCalled();
  });
});

// GET /api/v1/site-config is operatorOnly (R1): it carries the upstream
// proxy secret ref, every integration's credential ref and the internal
// proxy/SCM hostnames, so a plain member must never receive it on this page
// load. A member reaches this screen with siteConfig === null because the
// read is skipped outright (adminReads above), not because it still fires
// and 403s into a caught null.
//
// Absence is fine; a false STATEMENT is not. With a null config `isProxyConfigured`
// is false, so the two places that assert a proxy POSTURE would tell a member of
// a proxied deployment that sandboxes "go direct" — wrong, not redacted. Both are
// operator-only now, and this pins BOTH directions: an operator still sees them,
// so the gate cannot be satisfied by deleting the feature.
describe("SettingsScreen — the proxy posture is operator-only", () => {
  it("hides the proxy posture from a member rather than telling them it is not configured", async () => {
    getSiteConfigMock.mockResolvedValue(null);
    renderScreen(false);
    await screen.findAllByText("Host");
    expect(screen.queryByText(/sandboxes go direct/i)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/corporate proxy & egress/i),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/^Internet$/)).not.toBeInTheDocument();
  });

  it("still shows an operator the proxy posture, including the configured URL", async () => {
    getSiteConfigMock.mockResolvedValue({
      upstream_proxy_url: "http://proxy.internal.corp.example:3128",
    });
    renderScreen(true);
    expect(
      await screen.findByText(/corporate proxy & egress/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/proxy\.internal\.corp\.example:3128/),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Internet$/)).toBeInTheDocument();
  });
});

// R4/F069 — the operator's own FAILED read is the case the operator-only gate
// above does not cover, and the operator is the person these two sentences are
// addressed to. SettingsScreen.load swallows every site-config failure into
// `null` and still resolves state="ready", so a 500 or a dead network produced
// "Internet: Direct" and "Not configured — sandboxes go direct" — two POSITIVE
// claims about a deployment nothing had read. On a proxied deployment whose
// site-config endpoint is briefly down, both are false, and "sandboxes go
// direct" is false in the direction an operator acts on.
//
// The rule the file already applies to members, one state further: absence is
// honest, a statement is not. The funnel link itself stays — it is how the
// operator goes and finds out.
describe("SettingsScreen — a FAILED site-config read is not a proxy posture", () => {
  it("claims neither 'Direct' nor 'Not configured' when the read failed", async () => {
    getSiteConfigMock.mockRejectedValue(new Error("HTTP 500 store unavailable"));
    renderScreen(true);

    // The screen still renders — a site-config failure is not a page failure.
    expect(
      await screen.findByRole("heading", { name: "Host", level: 3 }),
    ).toBeInTheDocument();
    // The way in is still offered…
    expect(screen.getByText(/corporate proxy & egress/i)).toBeInTheDocument();
    // …but nothing on the card states a posture nothing read.
    expect(screen.queryByText(/sandboxes go direct/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Internet$/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Direct$/)).not.toBeInTheDocument();
    expect(screen.queryByText(/through the corporate proxy/i)).not.toBeInTheDocument();
  });

  // The negative control, so the fix cannot be satisfied by deleting the
  // feature: a server that ANSWERS "nothing is configured" is a real answer and
  // still says so.
  it("an answered empty config still says 'Not configured' — that one is a fact", async () => {
    getSiteConfigMock.mockResolvedValue({});
    renderScreen(true);
    expect(
      await screen.findByText(/not configured — sandboxes go direct/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Internet$/)).toBeInTheDocument();
    expect(screen.getByText(/^Direct$/)).toBeInTheDocument();
  });

  // …and a retry that succeeds must recover: the failure state is per-load, not
  // sticky. "Re-check this host" is the control that drives it.
  it("recovers on a re-check that succeeds", async () => {
    getSiteConfigMock
      .mockRejectedValueOnce(new Error("HTTP 500 store unavailable"))
      .mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.example:3128" });
    renderScreen(true);
    expect(
      await screen.findByRole("heading", { name: "Host", level: 3 }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/^Internet$/)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /re-check this host/i }));
    expect(await screen.findByText(/^Internet$/)).toBeInTheDocument();
    expect(screen.getByText(/proxy\.corp\.example:3128/)).toBeInTheDocument();
  });
});

// X3-F1 (second symptom): a member reaches /settings from the account menu and
// the BarrierChip link, and their /setup/status body carries `checks: []`
// because redactSetupStatusForMember stripped it — not because this deployment
// has no image builder. The row read the absence as a fact and told them the
// per-run builder was Off. `checks_redacted` is the server saying which it is.
describe("SettingsScreen — a redacted checks list is not an Off image builder", () => {
  it("hides the Image builder row on a member's redacted body, and keeps it for an operator", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ checks: [], checks_redacted: true }));
    renderScreen(false);
    await screen.findByRole("heading", { name: "Host", level: 3 });
    expect(screen.queryByText("Image builder")).toBeNull();
    expect(screen.queryByText(/devcontainer builds and --image wraps are unavailable/)).toBeNull();

    // Negative control, same render path: an operator whose builder really IS
    // off still gets the row and the honest Off sentence.
    cleanup();
    getSetupStatusMock.mockResolvedValue(baseStatus({ checks: [] }));
    renderScreen();
    await screen.findByRole("heading", { name: "Host", level: 3 });
    expect(screen.getByText("Image builder")).toBeInTheDocument();
    expect(screen.getByText(/devcontainer builds and --image wraps are unavailable/)).toBeInTheDocument();
  });
});

// The Host card mounts the SAME picker (EnvironmentStep) Getting Started
// does. This pins a read-only statement of the server's own default, gated
// on installed availability exactly like every other selector — the card
// owns no private per-mount override state of its own (no localStorage).
// DONE WHEN: a test fails if an unavailable class becomes selectable again.
describe("SettingsScreen — the Host card's barrier picker offers only what's installed", () => {
  it("checks the strongest installed tier and disables the rest, read-only", async () => {
    // baseStatus: CC1+CC2 installed, CC3 not — Wall is the strongest.
    renderScreen();
    expect(
      await screen.findByRole("radio", { name: /Wall/, checked: true }),
    ).toBeInTheDocument();
    const vault = screen.getByRole("radio", { name: /Vault/ });
    expect(vault).toBeDisabled();
    // A click changes nothing: the card is a read-only statement of the
    // server's own default, never a second place that picks one.
    await userEvent.click(vault);
    expect(screen.getByRole("radio", { name: /Wall/, checked: true })).toBeInTheDocument();
    expect(vault).toHaveAttribute("aria-checked", "false");
  });

  it("disables Wall and Vault on a CC1-only host, and checks Fence", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"] } }),
    );
    renderScreen();
    expect(
      await screen.findByRole("radio", { name: /Fence/, checked: true }),
    ).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Wall/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Vault/ })).toBeDisabled();
  });
});
