/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view Settings (M-5, #636) — the admin-only half of the settings
// split. Was settings-screen.test.tsx until M-5 split the page into this file
// and your-account-screen.test.tsx (issue #636's own "Check": "the settings
// specs split per view").
//
// S-5 changes what "not an operator" MEANS here: the old unsplit page was
// reachable by a security admin too, and rendered a redacted, partial page for
// one. This page now REFUSES that tier outright (nothing fetched, see the S-5
// suite below), so the MEMBER/security-admin halves of the old file's
// partial-render suites (proxy posture hidden from a member, redacted checks
// hidden from a member, the operatorResolved cold-load guard for a member) no
// longer describe a reachable state and are dropped. The OPERATOR halves of
// those same suites are still reachable — a real super admin is the only
// caller who ever renders this page now — so they are ported below unchanged,
// alongside the Host card's own #1228/#1200 coverage.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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

const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));

const listKeysMock = vi.fn();
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: {
    listKeys: (...a: unknown[]) => listKeysMock(...a),
    addKey: vi.fn(),
    deleteKey: vi.fn(),
  },
}));

const getApprovalNotifyStatusMock = vi.fn();
vi.mock("../../../lib/api/approval-notify", () => ({
  approvalNotify: { getStatus: (...a: unknown[]) => getApprovalNotifyStatusMock(...a) },
}));

vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: vi.fn(), deleteSecret: vi.fn() },
}));

const getModelProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: () => getModelProvidersMock() },
}));
vi.mock("../../../lib/api/agent-providers", () => ({
  agentProviders: { getAgentProviders: () => Promise.resolve({ providers: {}, etag: null }) },
}));

// The model card's sign-in pane drives a real PTY through xterm, which does not
// render in jsdom.
vi.mock("./harness-login-pane", () => ({
  HarnessLoginPane: () => <div data-testid="login-pane" />,
}));

import { AdminSettingsScreen } from "./admin-settings-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import { ADMIN_SSH_KEYS } from "./admin-ssh-keys-card";
import { APPROVAL_NOTIFY } from "../../wardyn/copy/approval-notify";
import { SETTINGS_SUPER_ONLY, VIEW_REFUSAL } from "../../wardyn/copy/console-view";
import { OperatorProvider } from "../../wardyn/operator-context";
import { expandCard, startsWith } from "../../../lib/test-dom";

function renderScreen(operator = true, operatorResolved = true) {
  return render(
    <MemoryRouter initialEntries={["/admin/settings"]}>
      <OperatorProvider operator={operator} operatorResolved={operatorResolved} securityOperator>
        <AdminSettingsScreen />
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
  getModelProvidersMock.mockReset().mockResolvedValue({ providers: {}, connected: {}, etag: null });
  listKeysMock.mockReset().mockResolvedValue([]);
  getApprovalNotifyStatusMock.mockReset().mockResolvedValue({ channels: [] });
});

describe("AdminSettingsScreen", () => {
  it("draws Host, Model providers, Providers, User drives, Admin SSH keys, Approval notifications — in that order", async () => {
    renderScreen();
    await screen.findByTestId("user-drives-card");
    const html = document.body.innerHTML;
    const positions = [
      "Host",
      MODEL_PROVIDERS.TITLE,
      "Workspace providers",
      "User drives",
      ADMIN_SSH_KEYS.TITLE,
      APPROVAL_NOTIFY.TITLE,
    ].map((label) => html.indexOf(`>${label}<`));
    for (const p of positions) expect(p).toBeGreaterThan(-1);
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
  });

  // M-5 (#636): Admin SSH keys (S-1) is drawn after User drives (§1a). notify-e4
  // (packet M6 S3) adds Approval notifications as the seventh and now LAST card,
  // directly after it. Your account has no Drives card at all, so there is
  // nothing left for the old "drives is the last card" rule to protect there.
  it("draws Approval notifications LAST, after Admin SSH keys", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(APPROVAL_NOTIFY.TITLE) });
    const card = heading.closest("section")!;
    expect(card.parentElement?.lastElementChild).toBe(card);
    const sshCard = (await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) })).closest("section")!;
    // Host, Branding, Model providers (list), Providers, User drives, Admin
    // SSH keys, Approval notifications.
    expect(card.parentElement?.children).toHaveLength(7);
    expect(card.previousElementSibling).toBe(sshCard);
  });

  // The retired Model provider card is replaced by the Model providers list;
  // its pointer sentence was never approved canon.
  it("draws the Model providers list and no Model provider card", async () => {
    renderScreen();
    await screen.findByTestId("model-providers-list");
    await expandCard(MODEL_PROVIDERS.TITLE);
    expect(await screen.findByText(MODEL_PROVIDERS.EMPTY_TITLE)).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(">Model provider<");
    expect(screen.queryByText(/set up as model providers/i)).not.toBeInTheDocument();
  });
});

// S-5 (#636, approved 2026-09-27): a security admin has no Settings in their
// nav (app-shell.tsx's navItemsForView), so this only ever renders from a
// stale/typed link. DONE WHEN: a test fails if the refusal disappears, or if
// a security admin's visit fires either read.
describe("AdminSettingsScreen — S-5, a security admin is refused (nothing fetched)", () => {
  it("shows the refusal, names the tier, and asks neither /setup/status nor /site-config", async () => {
    renderScreen(/* operator */ false, /* operatorResolved */ true);
    expect(await screen.findByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeInTheDocument();
    expect(screen.getByText(SETTINGS_SUPER_ONLY.BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: SETTINGS_SUPER_ONLY.CTA })).toBeInTheDocument();
    expect(getSetupStatusMock).not.toHaveBeenCalled();
    expect(getSiteConfigMock).not.toHaveBeenCalled();
  });

  // Negative control: a real super admin gets the page, not the refusal.
  it("a super admin gets the real page", async () => {
    renderScreen(true, true);
    expect(await screen.findByRole("heading", { name: startsWith("Host"), level: 3 })).toBeInTheDocument();
    expect(screen.queryByText(SETTINGS_SUPER_ONLY.BODY)).not.toBeInTheDocument();
    expect(getSetupStatusMock).toHaveBeenCalled();
  });

  // The fail-open cold-load window (operatorResolved still false, `operator`
  // reading its TRUE default) must render the real page, not the refusal —
  // an unresolved /me must never lock an admin out of their own console.
  it("an unresolved /me (fail-open default) is not refused", async () => {
    renderScreen(true, /* operatorResolved */ false);
    expect(await screen.findByRole("heading", { name: startsWith("Host"), level: 3 })).toBeInTheDocument();
    expect(screen.queryByText(SETTINGS_SUPER_ONLY.BODY)).not.toBeInTheDocument();
  });
});

// S-1 (#636, approved 2026-09-27): the Admin SSH keys card — the console door
// for an override key now that Your account is the only place left to add
// ANY key. DONE WHEN: a test fails if the card stops filtering to role:"admin"
// (an admin who added a plain key would see it twice — here and in Your
// account — if this regressed to listing every key unfiltered).
describe("AdminSettingsScreen — Admin SSH keys (S-1)", () => {
  it("lists only the caller's admin-stamped keys, empty state otherwise", async () => {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:aaa", name: "laptop", public_key: "", role: "user", created_at: new Date().toISOString() },
    ]);
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) });
    const card = within(heading.closest("section")!);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await card.findByText(ADMIN_SSH_KEYS.EMPTY_TITLE)).toBeInTheDocument();
    expect(card.queryByText("laptop")).not.toBeInTheDocument();
  });

  it("shows an admin-stamped key with its Admin override chip", async () => {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:bbb", name: "break-glass laptop", public_key: "", role: "admin", created_at: new Date().toISOString() },
    ]);
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) });
    const card = within(heading.closest("section")!);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await card.findByText("break-glass laptop")).toBeInTheDocument();
    expect(card.getByText("Admin override")).toBeInTheDocument();
    expect(card.queryByText(ADMIN_SSH_KEYS.EMPTY_TITLE)).not.toBeInTheDocument();
  });

  it("Add key opens the shared dialog and reloads the list on success", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) });
    const card = within(heading.closest("section")!);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    await userEvent.click(card.getByRole("button", { name: /add key/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Add key" })).toBeInTheDocument();
  });

  // review R2-L1: the packet has no error row for this card, and the
  // expanded body's own ErrorState already renders "Something went wrong" —
  // showing it as the collapsed summary too would put the same string on
  // screen twice at once.
  it("a failed key list carries no collapsed summary, and the expanded body's own error heading is not repeated", async () => {
    listKeysMock.mockRejectedValue(new Error("HTTP 500"));
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) });
    const card = within(heading.closest("section")!);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await card.findByText("Something went wrong")).toBeInTheDocument();
    expect(card.getAllByText("Something went wrong")).toHaveLength(1);
  });
});

// #1200 — the Host card mounts the shared TierPicker in display mode: unchanged
// by the split, carried over from settings-screen.test.tsx.
describe("AdminSettingsScreen — the Host card's barrier picker offers only what's installed", () => {
  it("lists only the installed tiers, read-only — Vault is dropped entirely, not disabled", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    const hostCard = within(heading.closest("section")!);
    await expandCard("Host");
    expect(hostCard.queryByRole("radiogroup")).toBeNull();
    expect(hostCard.getAllByRole("status").map((el) => el.textContent)).toEqual([
      expect.stringContaining("Fence"),
      expect.stringContaining("Wall"),
    ]);
    expect(hostCard.queryByText("Vault")).toBeNull();
  });

  it("a CC1-only host lists Fence alone", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"] } }),
    );
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    const hostCard = within(heading.closest("section")!);
    await expandCard("Host");
    expect(hostCard.getAllByRole("status")).toHaveLength(1);
    expect(hostCard.getByText("Fence")).toBeInTheDocument();
    expect(hostCard.queryByText("Wall")).toBeNull();
    expect(hostCard.queryByText("Vault")).toBeNull();
  });
});

// The Host card is a SECOND mount of EnvironmentStep's own no-runner/k8s
// facts, and must keep them byte-identical rather than a compact-picker-only
// fallback with no fix line.
// DONE WHEN: a test fails if the canon "No sandbox runner" card, its fix
// line, or the k8s rows disappear from this card again.
describe("AdminSettingsScreen — the Host card keeps the canon no-runner card and the k8s rows", () => {
  it("a driver:'none' host gets the canon card and the operator fix line, and no TierPicker fallback", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "none", confinement_classes: [] } }),
    );
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    const hostCard = within(heading.closest("section")!);
    await expandCard("Host");
    expect(hostCard.getByText("No sandbox runner — runs can't launch.")).toBeInTheDocument();
    expect(hostCard.getByText(/-runner docker/)).toBeInTheDocument();
    // Never the compact-picker's own generic fallback beside the real card.
    expect(hostCard.queryByText(/no barrier is installed/i)).toBeNull();
  });

  it("a real Docker daemon that is simply down gets the daemon fix line, not the operator one", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "docker", confinement_classes: [] } }),
    );
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    const hostCard = within(heading.closest("section")!);
    await expandCard("Host");
    expect(hostCard.getByText("No sandbox runner — runs can't launch.")).toBeInTheDocument();
    expect(
      hostCard.getByText(/start the Docker daemon.*so Wardyn can build a barrier/),
    ).toBeInTheDocument();
    expect(hostCard.queryByText(/-runner docker/)).toBeNull();
  });

  it("a k8s driver gets the Runner/Egress-containment rows, even with zero classes", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "k8s", confinement_classes: [] } }),
    );
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    const hostCard = within(heading.closest("section")!);
    await expandCard("Host");
    expect(hostCard.getByText("Kubernetes")).toBeInTheDocument();
    expect(hostCard.getByText("Egress containment")).toBeInTheDocument();
    // Zero classes on a k8s driver is still "no runner" by the shared rule.
    expect(hostCard.getByText("No sandbox runner — runs can't launch.")).toBeInTheDocument();
  });
});

// GET /api/v1/site-config is operatorOnly: it carries the upstream proxy
// secret ref, every integration's credential ref and the internal proxy/SCM
// hostnames. A super admin — the only caller who ever reaches this page now
// — still sees the real posture, including the configured URL.
describe("AdminSettingsScreen — the proxy posture", () => {
  it("shows the operator the proxy posture, including the configured URL", async () => {
    getSiteConfigMock.mockResolvedValue({
      upstream_proxy_url: "http://proxy.internal.corp.example:3128",
    });
    renderScreen(true);
    await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    await expandCard("Host");
    expect(
      await screen.findByText(/corporate proxy & egress/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/proxy\.internal\.corp\.example:3128/),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Internet$/)).toBeInTheDocument();
  });
});

// A failed site-config read is not the same fact as it answering "nothing is
// configured" — AdminSettingsScreen.load swallows every site-config failure
// into `null` and still resolves state="ready", so a 500 or a dead network
// must not paint "Internet: Direct" or "Not configured — sandboxes go
// direct": two POSITIVE claims about a deployment nothing had read. Absence
// is honest; a statement is not. The funnel link itself stays — it is how the
// operator goes and finds out.
describe("AdminSettingsScreen — a FAILED site-config read is not a proxy posture", () => {
  it("claims neither 'Direct' nor 'Not configured' when the read failed", async () => {
    getSiteConfigMock.mockRejectedValue(new Error("HTTP 500 store unavailable"));
    renderScreen(true);

    // The screen still renders — a site-config failure is not a page failure.
    expect(
      await screen.findByRole("heading", { name: startsWith("Host"), level: 3 }),
    ).toBeInTheDocument();
    await expandCard("Host");
    // The way in is still offered…
    expect(screen.getByText(/corporate proxy & egress/i)).toBeInTheDocument();
    // …but nothing on the card states a posture nothing read.
    expect(screen.queryByText(/sandboxes go direct/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Internet$/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Direct$/)).not.toBeInTheDocument();
    expect(screen.queryByText(/through the corporate proxy/i)).not.toBeInTheDocument();
  });

  // The negative control, so the fix cannot be satisfied by deleting the
  // feature: a server that ANSWERS "nothing is configured" is a real answer
  // and still says so.
  it("an answered empty config still says 'Not configured' — that one is a fact", async () => {
    getSiteConfigMock.mockResolvedValue({});
    renderScreen(true);
    await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    await expandCard("Host");
    expect(
      await screen.findByText(/not configured — sandboxes go direct/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Internet$/)).toBeInTheDocument();
    expect(screen.getByText(/^Direct$/)).toBeInTheDocument();
  });

  // …and a retry that succeeds must recover: the failure state is per-load,
  // not sticky. "Re-check this host" is the control that drives it.
  it("recovers on a re-check that succeeds", async () => {
    getSiteConfigMock
      .mockRejectedValueOnce(new Error("HTTP 500 store unavailable"))
      .mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.example:3128" });
    renderScreen(true);
    expect(
      await screen.findByRole("heading", { name: startsWith("Host"), level: 3 }),
    ).toBeInTheDocument();
    await expandCard("Host");
    expect(screen.queryByText(/^Internet$/)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /re-check this host/i }));
    expect(await screen.findByText(/^Internet$/)).toBeInTheDocument();
    expect(screen.getByText(/proxy\.corp\.example:3128/)).toBeInTheDocument();
  });
});

// checks_redacted marks a body whose checks list was stripped for a member's
// tier — absent for an operator. An operator whose builder really IS off
// still gets the row and the honest Off sentence: this pins the row survives
// for the only caller who now reaches this page.
describe("AdminSettingsScreen — an operator's Image builder row", () => {
  it("keeps the row and the honest Off sentence when the builder really is off", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ checks: [] }));
    renderScreen();
    await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    await expandCard("Host");
    expect(screen.getByText("Image builder")).toBeInTheDocument();
    expect(screen.getByText(/devcontainer builds and --image wraps are unavailable/)).toBeInTheDocument();
  });
});
