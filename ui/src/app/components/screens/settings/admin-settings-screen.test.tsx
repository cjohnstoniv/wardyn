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
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { CARD_STATUS, SETTINGS_GROUPS } from "../../wardyn/copy/card-status";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

const getSiteConfigMock = vi.fn();
const getSiteConfigSnapshotMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    getSiteConfigSnapshot: (...a: unknown[]) => getSiteConfigSnapshotMock(...a),
  },
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

const getScimStatusMock = vi.fn();
vi.mock("../../../lib/api/scim", () => ({
  scim: { getStatus: () => getScimStatusMock() },
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
import { SCIM } from "../../../lib/scim-copy";
import { SETTINGS_SUPER_ONLY, VIEW_REFUSAL } from "../../wardyn/copy/console-view";
import { HOST_CARD } from "../../wardyn/copy/host-card";
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
  getSiteConfigSnapshotMock.mockReset().mockResolvedValue({ siteConfig: {}, etag: null });
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
  getScimStatusMock.mockReset().mockResolvedValue({
    configured: false,
    last_token_slot: "",
    purge_after_seconds: 0,
    keep_workspaces: false,
    deactivated: [],
    pending: [],
    drives: [],
  });
});

describe("AdminSettingsScreen", () => {
  it("draws Host, Model providers, Providers, Custom components, User drives, Admin SSH keys, Approval notifications, SCIM provisioning — in that order", async () => {
    renderScreen();
    await screen.findByTestId("user-drives-card");
    const html = document.body.innerHTML;
    const positions = [
      "Host",
      MODEL_PROVIDERS.TITLE,
      "Workspace providers",
      "Custom components",
      "User drives",
      ADMIN_SSH_KEYS.TITLE,
      APPROVAL_NOTIFY.TITLE,
      SCIM.TITLE,
    ].map((label) => html.indexOf(`>${label}<`));
    for (const p of positions) expect(p).toBeGreaterThan(-1);
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
  });

  // #3048: the nine cards read as three places, a blocking state stands out, and a card that only
  // links elsewhere is a link.
  it("groups the cards under This host, Connections and People and access", async () => {
    renderScreen();
    await screen.findByTestId("user-drives-card");
    const labels = SETTINGS_GROUPS;
    const groupOf = (testId: string) => screen.getByTestId(testId).closest("[role=group]")?.getAttribute("aria-label");
    expect(groupOf("host-card")).toBe(labels.HOST);
    expect(groupOf("model-providers-list")).toBe(labels.CONNECTIONS);
    expect(groupOf("user-drives-card")).toBe(labels.CONNECTIONS);
    expect(groupOf("approval-notify-card")).toBe(labels.PEOPLE);
    expect(groupOf("scim-card")).toBe(labels.PEOPLE);
  });

  it("states the blocking Host condition as a pill, and links Workspace providers and User drives as rows", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "none", confinement_classes: [] } }));
    renderScreen();
    await screen.findByTestId("user-drives-card");
    expect(await screen.findByTestId("host-card-status")).toHaveTextContent(CARD_STATUS.CANT_LAUNCH);
    expect(screen.getByTestId("model-providers-list-status")).toHaveTextContent(CARD_STATUS.NOT_SET_UP);
    for (const [id, href] of [
      ["providers-card", "/admin/providers"],
      ["user-drives-card", "/admin/drives"],
    ]) {
      const card = screen.getByTestId(id);
      expect(within(card).getByRole("link")).toHaveAttribute("href", href);
      expect(within(card).queryByRole("button")).toBeNull();
    }
  });

  // M-5 (#636): Admin SSH keys (S-1) is drawn after User drives (§1a). notify-e4
  // (packet M6 S3) adds Approval notifications directly after it, and 0.8.6 scim-a7
  // (M5) puts SCIM provisioning after that, the LAST card. Your account has no Drives
  // card at all, so there is nothing left for the old "drives is the last card" rule
  // to protect there.
  it("draws Admin SSH keys, then Approval notifications, then SCIM provisioning LAST", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(APPROVAL_NOTIFY.TITLE) });
    const card = heading.closest("section")!;
    const sshCard = (await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) })).closest("section")!;
    const drivesCard = await screen.findByTestId("user-drives-card");
    const scimCard = await screen.findByTestId("scim-card");
    // The cards sit in three groups, but their order down the page is the same.
    const before = (a: Element, b: Element) => !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(before(drivesCard, sshCard)).toBe(true);
    expect(before(sshCard, card)).toBe(true);
    expect(before(card, scimCard)).toBe(true);
    // SCIM is the last card on the page.
    expect(card.parentElement?.lastElementChild).toBe(scimCard);
    // Custom components sits between the providers and the drives (0.8.9).
    const componentsCard = await screen.findByTestId("components-settings-card");
    expect(componentsCard.nextElementSibling).toBe(drivesCard);
    expect(drivesCard.nextElementSibling).toBe(sshCard);
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
    expect(await screen.findByRole("heading", { name: SETTINGS_SUPER_ONLY.TITLE })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: VIEW_REFUSAL.TITLE })).not.toBeInTheDocument();
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

  // #2873: inside a card the empty state is the quiet one: a level-4 title in the standard icon tile.
  it("an empty card says so with a quiet level-4 title in the icon tile", async () => {
    listKeysMock.mockResolvedValue([]);
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith(ADMIN_SSH_KEYS.TITLE) });
    const card = within(heading.closest("section")!);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await card.findByRole("heading", { name: ADMIN_SSH_KEYS.EMPTY_TITLE, level: 4 })).toBeInTheDocument();
    expect(card.getByTestId("empty-state-tile")).toBeInTheDocument();
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
  // not sticky. "Re-check" is the control that drives it.
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

    await userEvent.click(screen.getByRole("button", { name: /^re-check$/i }));
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

// #2876 (owner option A): the Host card's three facts are status tiles, a state pill says the whole host at a glance,
// and the sentences are the console's own, split at their dash.
describe("AdminSettingsScreen — the Host card's status tiles", () => {
  const BUILDER_OK = { id: "env_builder", label: "Sandbox image builder", status: "ok" as const };
  const openHost = async () => {
    await screen.findByRole("heading", { name: startsWith("Host"), level: 3 });
    await expandCard("Host");
  };

  it("a broken host says Can't launch in red; each fact is a tile with a coloured word and its sub-line", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "none", confinement_classes: [] } }));
    renderScreen();
    await openHost();
    expect(within(screen.getByTestId("host-state-pill")).getByText(HOST_CARD.CANT_LAUNCH)).toBeInTheDocument();
    expect(screen.getByTestId("host-state-pill")).toHaveAttribute("data-tone", "danger");
    expect(screen.getByTestId("host-state-tile")).toHaveAttribute("data-tone", "danger");
    expect(screen.getByTestId("host-tile-image-builder-pill")).toHaveTextContent(/^Off$/);
    expect(screen.getByTestId("host-tile-image-builder-pill")).toHaveAttribute("data-tone", "warning");
    expect(screen.getByTestId("host-tile-image-builder-sub")).toHaveTextContent(
      "devcontainer builds and --image wraps are unavailable",
    );
    expect(screen.getByTestId("host-tile-recording-store-pill")).toHaveTextContent(/^Ephemeral$/);
    expect(screen.getByTestId("host-tile-recording-store-pill")).toHaveAttribute("data-tone", "warning");
    expect(screen.getByTestId("host-tile-recording-store-sub")).toHaveTextContent("recordings are lost on restart");
    expect(screen.getByTestId("host-tile-internet-pill")).toHaveTextContent(/^Direct$/);
    expect(screen.getByTestId("host-tile-internet-pill")).toHaveAttribute("data-tone", "neutral");
    expect(screen.queryByTestId("host-tile-internet-sub")).toBeNull();
  });

  it("a healthy host says what runs default to, in green, and its good facts are green with no sub-line", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ checks: [BUILDER_OK], age_key: { durable: true } }));
    getSiteConfigMock.mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.example:3128" });
    renderScreen();
    await openHost();
    expect(screen.getByTestId("host-state-pill")).toHaveTextContent(/^Runs default to /);
    expect(screen.getByTestId("host-state-pill")).toHaveAttribute("data-tone", "success");
    expect(screen.getByTestId("host-tile-image-builder-pill")).toHaveTextContent(/^Wired$/);
    expect(screen.getByTestId("host-tile-recording-store-pill")).toHaveTextContent(/^Enabled$/);
    expect(screen.getByTestId("host-tile-internet-pill")).toHaveTextContent(/^Through the corporate proxy$/);
    for (const id of ["image-builder", "recording-store", "internet"]) {
      expect(screen.getByTestId(`host-tile-${id}-pill`)).toHaveAttribute("data-tone", "success");
      expect(screen.queryByTestId(`host-tile-${id}-sub`)).toBeNull();
    }
  });

  // #2548: the first load (before /me resolved, so without the site-config read) can answer after the second.
  it("a slow first load cannot overwrite the resolved one and claim a proxied host goes direct", async () => {
    let answerFirst!: (s: ReturnType<typeof baseStatus>) => void;
    getSetupStatusMock.mockReset().mockReturnValueOnce(new Promise((r) => (answerFirst = r))).mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.example:3128" });
    const tree = (resolved: boolean) => (
      <MemoryRouter initialEntries={["/admin/settings"]}>
        <OperatorProvider operator operatorResolved={resolved} securityOperator>
          <AdminSettingsScreen />
        </OperatorProvider>
      </MemoryRouter>
    );
    const { rerender } = render(tree(false));
    rerender(tree(true));
    await openHost();
    expect(screen.getByTestId("host-tile-internet-pill")).toHaveTextContent(/^Through the corporate proxy$/);

    await act(async () => answerFirst(baseStatus()));
    expect(screen.getByTestId("host-tile-internet-pill")).toHaveTextContent(/^Through the corporate proxy$/);
  });

  it("keeps its gating: no builder tile on a redacted body, no Internet tile when the read failed", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ checks_redacted: true }));
    getSiteConfigMock.mockRejectedValue(new Error("HTTP 500"));
    renderScreen();
    await openHost();
    expect(screen.queryByTestId("host-tile-image-builder")).toBeNull();
    expect(screen.queryByTestId("host-tile-internet")).toBeNull();
    expect(screen.getByTestId("host-tile-recording-store")).toBeInTheDocument();
  });

  it("puts the proxy link and Re-check in one footer", async () => {
    renderScreen();
    await openHost();
    const footer = within(screen.getByTestId("host-footer"));
    expect(footer.getByRole("button", { name: /corporate proxy & egress/i })).toBeInTheDocument();
    expect(footer.getByRole("button", { name: /^re-check$/i })).toBeInTheDocument();
  });
});
