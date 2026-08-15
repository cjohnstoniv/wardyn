/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { AppShell, MobileNav } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";

// below md the desktop aside is hidden, so this Sheet-based hamburger is
// the ONLY navigation. These pins fail if the drawer stops opening, drops nav
// items, or loses its aria-expanded/Escape wiring.
function renderMobileNav(role: "admin" | "member" = "admin") {
  return render(
    <MemoryRouter>
      <MobileNav
        pendingApprovals={2}
        attentionCount={0}
        readiness="ready"
        meta={{
          trustDomain: "example.test",
          identityProvider: "spiffe",
          principal: "u@example.test",
          method: "sso",
          operator: role === "admin",
          role,
        }}
      />
    </MemoryRouter>,
  );
}

// Every background poll in the console keeps its last-good data on failure, so
// this banner is the ONLY thing separating a quiet fleet from a dead daemon —
// and the readiness chip must not report the outage as an unfinished setup.
describe("AppShell (control plane unreachable)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShell(unreachable: boolean) {
    const fetchMock = vi.fn().mockRejectedValue(new Error("connection refused"));
    vi.stubGlobal("fetch", fetchMock);
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
            unreachable={unreachable}
            lastOkAt={null}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
    return fetchMock;
  }

  it("stays silent while the daemon answers", () => {
    renderShell(false);
    expect(screen.queryByText(/Control plane unreachable/)).toBeNull();
  });

  it("banners the outage and leaves the readiness chip alone", async () => {
    const fetchMock = renderShell(true);
    expect(screen.getByText(/Control plane unreachable/)).toBeInTheDocument();
    // Its own status probe resolves the synthetic unreachable payload; the chip
    // must NOT read that as "Needs setup".
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith("/api/v1/setup/status", expect.anything()),
    );
    expect(screen.queryByText("Needs setup")).toBeNull();
    expect(screen.getByText(/Checking/)).toBeInTheDocument();
  });
});

// L1 review fix: the role chip used to render unconditionally (fail-open
// "admin"), so it flashed ADMIN in the account menu next to a still-"unknown"
// principal before /me resolves — or forever, if /me never resolves at all.
// Gated on meta.method now, same as its sibling line just below it.
describe("AppShell — account-menu role chip gating (L1)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("never shows the role chip while /me hasn't resolved (a permanently failing fetch)", async () => {
    const fetchMock = vi.fn().mockRejectedValue(new Error("network down"));
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />
        </ThemeProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());

    // The account-menu trigger is the last button in the header (after "New
    // run" and "Toggle theme" — see TopBar).
    const header = screen.getByRole("banner");
    const headerButtons = within(header).getAllByRole("button");
    await user.click(headerButtons[headerButtons.length - 1]);

    const menu = screen.getByRole("menu");
    expect(within(menu).queryByText("admin", { exact: true })).toBeNull();
    expect(within(menu).queryByText("member", { exact: true })).toBeNull();
  });
});

// W31-S1-1: local-mode installs bypass auth entirely server-side
// (internal/api/http.go humanOrAdminAuth), so "Sign out" is a no-op that only
// drops the user onto a SignIn screen whose admin-token field is unchecked
// (probeAuth trivially re-succeeds against the auth-bypassed API on whatever's
// typed). The account menu must hide Sign out — and say why — whenever /me
// reports method:"local", while a real session (sso/token) keeps it.
describe("AppShell — Sign out hidden in local mode (W31-S1-1)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShellAs(method: "local" | "sso" | "token") {
    const fetchMock = vi.fn((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.endsWith("/healthz")) {
        return Promise.resolve({
          ok: true,
          json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
        });
      }
      if (u.endsWith("/api/v1/me")) {
        return Promise.resolve({
          ok: true,
          json: async () => ({
            principal: "local:operator",
            method,
            operator: true,
            role: "admin",
            email: "",
          }),
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({}) });
    });
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  async function openAccountMenu() {
    const header = screen.getByRole("banner");
    const headerButtons = within(header).getAllByRole("button");
    const user = userEvent.setup();
    await user.click(headerButtons[headerButtons.length - 1]);
    return screen.findByRole("menu");
  }

  it("hides Sign out and explains local mode when meta.method is local", async () => {
    renderShellAs("local");
    const menu = await openAccountMenu();
    await waitFor(() => expect(within(menu).getByText(/local mode/i)).toBeInTheDocument());
    expect(within(menu).queryByText("Sign out")).toBeNull();
  });

  it("still offers Sign out for a real SSO session", async () => {
    renderShellAs("sso");
    const menu = await openAccountMenu();
    await waitFor(() => expect(within(menu).getByText("Sign out")).toBeInTheDocument());
  });
});

// ui-shellAuth-4: the account-menu trigger must render via the shared Button
// component (like every sibling header control) so keyboard focus gets the
// app's focus-visible ring instead of falling back to a raw <button>'s bare
// unthemed browser-default outline.
describe("AppShell — account-menu trigger uses the shared Button (ui-shellAuth-4)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("carries the shared Button's focus-visible ring classes", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />
        </ThemeProvider>
      </MemoryRouter>,
    );
    const header = screen.getByRole("banner");
    const headerButtons = within(header).getAllByRole("button");
    const accountTrigger = headerButtons[headerButtons.length - 1];
    expect(accountTrigger).toHaveClass("focus-visible:ring-ring/50");
  });
});

describe("MobileNav (below-md nav fallback)", () => {
  it("starts collapsed: trigger present, aria-expanded=false, no nav links rendered", () => {
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "Runs" })).toBeNull();
  });

  it("opening the drawer reveals every nav item and flips aria-expanded to true", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    await user.click(trigger);

    expect(trigger).toHaveAttribute("aria-expanded", "true");
    for (const label of ["Runs", "Approvals", "Demos", "Policies", "Secrets", "Workspaces", "Audit", "Recordings", "Getting started"]) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
  });

  it("Escape closes the drawer and returns aria-expanded to false", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", { name: /open navigation menu/i });
    await user.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(trigger).toHaveAttribute("aria-expanded", "false"));
    expect(screen.queryByRole("link", { name: "Runs" })).toBeNull();
  });
});

// B3: member nav is Runs · Approvals · Demos · Recordings, nothing else — no
// Policies/Secrets/Workspaces/Audit/Getting started. Hiding is cosmetic (the
// server is the real boundary); this pins the UI half of that contract.
// ui-shellAuth-2: Demos is included on the member side because routes.go has
// no server-side gate on it at all — hiding it here would be a pure
// discoverability regression with nothing backing it, unlike its siblings.
describe("SidebarNav (member role — B3)", () => {
  it("shows Runs, Approvals, Demos, Recordings — admin-only items and Getting started are absent", async () => {
    const user = userEvent.setup();
    renderMobileNav("member");
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));

    for (const label of ["Runs", "Approvals", "Demos", "Recordings"]) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
    for (const label of ["Policies", "Secrets", "Integrations", "Workspaces", "Audit", "Getting started"]) {
      expect(screen.queryByRole("link", { name: new RegExp(`^${label}`) })).toBeNull();
    }
  });

  it("admin nav is unchanged: every item including Getting started is present", async () => {
    const user = userEvent.setup();
    renderMobileNav("admin");
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));

    for (const label of ["Runs", "Approvals", "Demos", "Policies", "Secrets", "Workspaces", "Audit", "Recordings", "Getting started"]) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
  });
});
