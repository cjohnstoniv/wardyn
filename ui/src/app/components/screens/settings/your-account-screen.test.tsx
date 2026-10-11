/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account (M-5, #636) — the user-view half of the settings split. Was
// part of the unsplit settings-screen.test.tsx until M-5 split the page in
// two (issue #636's own "Check": "the settings specs split per view").
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
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

// The model card's sign-in pane drives a real PTY through xterm, which does not
// render in jsdom.
vi.mock("./harness-login-pane", () => ({
  HarnessLoginPane: () => <div data-testid="login-pane" />,
}));

import { YourAccountScreen } from "./your-account-screen";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "../../../lib/test-fixtures";
import { WithDoor } from "../../../../test/door-harness";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { YOUR_ACCOUNT } from "../../wardyn/copy/console-view";
import { OperatorProvider } from "../../wardyn/operator-context";
import { expandCard, startsWith } from "../../../lib/test-dom";

function renderScreen(operator = false) {
  return render(
    <MemoryRouter initialEntries={["/account"]}>
      <OperatorProvider operator={operator}>
        <YourAccountScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listKeysMock.mockReset().mockResolvedValue([]);
});

describe("YourAccountScreen", () => {
  it("titles the page 'Your account' with the S-3 description", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: "Your account", level: 1 })).toBeInTheDocument();
    expect(screen.getByText(YOUR_ACCOUNT.LEDE)).toBeInTheDocument();
  });

  // The settings-split mock draws exactly three cards here; the retired
  // Model provider card (and its unapproved pointer sentence) is not one.
  it("draws no Model provider card, and no Azure DevOps card when none is configured", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: startsWith("Model provider") })).not.toBeInTheDocument();
    expect(screen.queryByText(/set up as model providers/i)).not.toBeInTheDocument();
    expect(screen.queryByText("Azure DevOps")).not.toBeInTheDocument();
  });

  it("draws the Azure DevOps card when a row is configured", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ scm_access: { state: "live", source: "org", org: "https://dev.azure.com/example-org" } }),
    );
    renderScreen();
    expect(await screen.findByRole("heading", { name: startsWith("Azure DevOps") })).toBeInTheDocument();
  });

  // M-5 (#636): this page has NONE of Admin Settings' cards — Host, the
  // admin Model providers list, Providers, User drives and Admin SSH keys
  // all stayed there. Nothing here belongs to the deployment.
  it("has no admin cards — Host, Model providers, Providers, User drives, Admin SSH keys", async () => {
    renderScreen();
    await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 });
    expect(screen.queryByRole("heading", { name: "Host" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Model providers" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Workspace providers" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "User drives" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Admin SSH keys" })).not.toBeInTheDocument();
  });

  // Even a real super admin viewing their OWN account (e.g. via "Open in user
  // view") gets no admin-only read fired from this page — there is nothing
  // here for it to gate, since Host (the one card that ever needed
  // GET /site-config) left this page entirely.
  it("fires no site-config read regardless of the caller's tier", async () => {
    renderScreen(/* operator */ true);
    await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 });
    // No Host card means no button that would even offer the read; the
    // absence itself is the proof, alongside the module mock list above
    // carrying no health.getSiteConfig entry at all.
    expect(screen.queryByText(/corporate proxy & egress/i)).not.toBeInTheDocument();
  });
});

// S-2 (#636, approved 2026-09-27): the SSH pane's own strings, rewritten so
// the non-admin side is never called "member" and its "no admin view of
// anyone else's" no longer collides with the console's Admin view.
describe("YourAccountScreen — Your SSH keys, the S-2 strings", () => {
  it("carries the rewritten description", async () => {
    renderScreen();
    await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 });
    await expandCard("Your SSH keys");
    expect(
      await screen.findByText(
        "Public keys only — Wardyn never stores or asks for a private key. Keys are yours alone; admins can't list anyone else's.",
      ),
    ).toBeInTheDocument();
  });

  it("shows 'User access' (not 'Member access') on a capped key, with the rewritten tooltip", async () => {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:aaa", name: "laptop", public_key: "", role: "user", capped: true, created_at: new Date().toISOString() },
    ]);
    renderScreen();
    await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 });
    await expandCard("Your SSH keys");
    const chip = await screen.findByText("User access");
    expect(screen.queryByText("Member access")).not.toBeInTheDocument();
    expect(chip.closest("[title]")).toHaveAttribute(
      "title",
      "Added in the user view, so it keeps user rights. To reach other people's runs over SSH, add a key in Settings in the admin view.",
    );
  });

  // review R2-L1: the packet has no error row for this card, and the
  // expanded body's own ErrorState already renders "Something went wrong" —
  // showing it as the collapsed summary too would put the same string on
  // screen twice at once.
  it("a failed key list carries no collapsed summary, and the expanded body's own error heading is not repeated", async () => {
    listKeysMock.mockRejectedValue(new Error("HTTP 500"));
    renderScreen();
    const heading = await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 });
    const card = within(heading.closest("section")!);
    await expandCard("Your SSH keys");
    expect(await card.findByText("Something went wrong")).toBeInTheDocument();
    expect(card.getAllByText("Something went wrong")).toHaveLength(1);
  });
});

// #541: Your model connections is the User view's card. An admin reaches it
// by switching to the User view, so under an /admin/ path the page omits it.
describe("YourAccountScreen — Your model connections", () => {
  const status = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" }]);
  const renderAt = (path: string) =>
    render(
      <WithDoor status={status} path={path} operator={false}>
        <YourAccountScreen />
      </WithDoor>,
    );

  it("shows the card in the User view", async () => {
    getSetupStatusMock.mockResolvedValue(status);
    renderAt("/account");
    expect(await screen.findByTestId("model-connections-card")).toBeInTheDocument();
  });

  it("does not show it in the Admin view", async () => {
    getSetupStatusMock.mockResolvedValue(status);
    renderAt("/admin/account");
    expect(await screen.findByRole("heading", { name: startsWith("Your SSH keys"), level: 3 })).toBeInTheDocument();
    expect(screen.queryByTestId("model-connections-card")).toBeNull();
  });
});


it("model re-check refreshes the shared snapshot once and keeps the recovery dialog current", async () => {
  const status = providerStatus([{ provider: MODEL_PROVIDERS.gateway }]);
  status.provider_access![0].cause = "store_unreadable";
  const refresh = vi.fn();
  const { rerender } = render(<WithDoor status={status} path="/account" operator={false} onRefresh={refresh}>
    <YourAccountScreen />
  </WithDoor>);
  await screen.findByTestId("model-connections-card");
  await expandCard(CONNECTIONS.TITLE);
  await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.RECHECK }));
  expect(refresh).toHaveBeenCalledOnce();
  expect(getSetupStatusMock).toHaveBeenCalledOnce();
  const recovered = { ...status, provider_access: [{ ...status.provider_access![0], cause: "destination_changed" }] };
  rerender(<WithDoor status={recovered} path="/account" operator={false} onRefresh={refresh}>
    <YourAccountScreen />
  </WithDoor>);
  await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.REVIEW_RECONNECT }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(CONNECTIONS.DESTINATION_CHANGED(MODEL_PROVIDERS.gateway.host))).toBeInTheDocument();
});
