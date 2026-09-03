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
import { render, screen } from "@testing-library/react";
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

function renderScreen(operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} securityOperator>
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
});

describe("SettingsScreen", () => {
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

// GET /api/v1/site-config became operatorOnly in R1: it carries the upstream
// proxy secret ref, every integration's credential ref and the internal
// proxy/SCM hostnames, and a plain member used to receive all of it on this
// page load. A member now reaches this screen with siteConfig === null (the
// fetch 403s and the screen already .catch()es into null).
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
