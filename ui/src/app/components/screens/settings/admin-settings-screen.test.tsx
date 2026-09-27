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
// one. This page now REFUSES that tier outright (nothing fetched), so the
// member/security-admin partial-render suites the old file carried (proxy
// posture hidden, redacted checks hidden, the operatorResolved cold-load
// guard) no longer describe a reachable state and are replaced by the refusal
// suite below.
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
import { SETTINGS_SUPER_ONLY, VIEW_REFUSAL } from "../../wardyn/copy/console-view";
import { OperatorProvider } from "../../wardyn/operator-context";

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
});

describe("AdminSettingsScreen", () => {
  it("draws Host, Model providers, Model provider, Providers, User drives, Admin SSH keys — in that order", async () => {
    renderScreen();
    await screen.findByTestId("user-drives-card");
    const html = document.body.innerHTML;
    const positions = [
      "Host",
      MODEL_PROVIDERS.TITLE,
      "Model provider",
      "Workspace providers",
      "User drives",
      ADMIN_SSH_KEYS.TITLE,
    ].map((label) => html.indexOf(`>${label}<`));
    for (const p of positions) expect(p).toBeGreaterThan(-1);
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
  });

  // M-5 (#636): Admin SSH keys (S-1) is the sixth and now LAST card — the
  // mock draws it after User drives (§1a). The pre-M-5 "drives is the fifth
  // and last card" invariant (user-drives-prompt.md §6) is superseded for
  // THIS page by the packet's own layout; Your account has no Drives card at
  // all, so there is nothing left for that rule to protect there either.
  it("draws Admin SSH keys LAST, after User drives", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: ADMIN_SSH_KEYS.TITLE });
    const card = heading.closest("section")!;
    expect(card.parentElement?.lastElementChild).toBe(card);
    const drivesCard = await screen.findByTestId("user-drives-card");
    // Host, Branding, Model providers (list), Model provider, Providers, User
    // drives, Admin SSH keys.
    expect(card.parentElement?.children).toHaveLength(7);
    // User drives sits directly before it.
    expect(card.previousElementSibling).toBe(drivesCard);
  });

  it("draws the Model providers list above the Model provider card", async () => {
    renderScreen();
    expect(await screen.findByText(MODEL_PROVIDERS.EMPTY_TITLE)).toBeInTheDocument();
    const html = document.body.innerHTML;
    expect(html.indexOf(`>${MODEL_PROVIDERS.TITLE}<`)).toBeLessThan(html.indexOf(">Model provider<"));
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
    expect(await screen.findByRole("heading", { name: "Host", level: 3 })).toBeInTheDocument();
    expect(screen.queryByText(SETTINGS_SUPER_ONLY.BODY)).not.toBeInTheDocument();
    expect(getSetupStatusMock).toHaveBeenCalled();
  });

  // The fail-open cold-load window (operatorResolved still false, `operator`
  // reading its TRUE default) must render the real page, not the refusal —
  // an unresolved /me must never lock an admin out of their own console.
  it("an unresolved /me (fail-open default) is not refused", async () => {
    renderScreen(true, /* operatorResolved */ false);
    expect(await screen.findByRole("heading", { name: "Host", level: 3 })).toBeInTheDocument();
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
    const heading = await screen.findByRole("heading", { name: ADMIN_SSH_KEYS.TITLE });
    const card = within(heading.closest("section")!);
    expect(card.getByText(ADMIN_SSH_KEYS.EMPTY_TITLE)).toBeInTheDocument();
    expect(card.queryByText("laptop")).not.toBeInTheDocument();
  });

  it("shows an admin-stamped key with its Admin override chip", async () => {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:bbb", name: "break-glass laptop", public_key: "", role: "admin", created_at: new Date().toISOString() },
    ]);
    renderScreen();
    const heading = await screen.findByRole("heading", { name: ADMIN_SSH_KEYS.TITLE });
    const card = within(heading.closest("section")!);
    expect(await card.findByText("break-glass laptop")).toBeInTheDocument();
    expect(card.getByText("Admin override")).toBeInTheDocument();
    expect(card.queryByText(ADMIN_SSH_KEYS.EMPTY_TITLE)).not.toBeInTheDocument();
  });

  it("Add key opens the shared dialog and reloads the list on success", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: ADMIN_SSH_KEYS.TITLE });
    const card = within(heading.closest("section")!);
    await userEvent.click(card.getByRole("button", { name: /add key/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Add key" })).toBeInTheDocument();
  });
});

// #1200 — the Host card mounts the shared TierPicker in display mode: unchanged
// by the split, carried over from settings-screen.test.tsx.
describe("AdminSettingsScreen — the Host card's barrier picker offers only what's installed", () => {
  it("lists only the installed tiers, read-only — Vault is dropped entirely, not disabled", async () => {
    renderScreen();
    const heading = await screen.findByRole("heading", { name: "Host", level: 3 });
    const hostCard = within(heading.closest("section")!);
    expect(hostCard.queryByRole("radiogroup")).toBeNull();
    expect(hostCard.getAllByRole("status").map((el) => el.textContent)).toEqual([
      expect.stringContaining("Fence"),
      expect.stringContaining("Wall"),
    ]);
    expect(hostCard.queryByText("Vault")).toBeNull();
  });
});
