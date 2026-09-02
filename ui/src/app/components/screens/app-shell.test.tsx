/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell, MobileNav, TopBar, useFocusMode } from "./app-shell";
import { useRoleResolved, type Role } from "../wardyn/operator-context";
import { ThemeProvider } from "../wardyn/theme-provider";
import type { ConfinementClass } from "../../lib/types";

// below md the desktop aside is hidden, so this Sheet-based hamburger is
// the ONLY navigation. These pins fail if the drawer stops opening, drops nav
// items, or loses its aria-expanded/Escape wiring.
// role is the full three-valued union since 0.7. operator/securityOperator
// mirror the server's two predicates exactly: "admin" is both, "security_admin"
// is only the second, "member" is neither.
function renderMobileNav(role: Role = "admin") {
  return render(
    <MemoryRouter>
      <MobileNav
        pendingApprovals={2}
        attentionCount={0}
        meta={{
          trustDomain: "example.test",
          identityProvider: "spiffe",
          principal: "u@example.test",
          method: "sso",
          resolved: true,
          operator: role === "admin",
          securityOperator: role !== "member",
          role,
          sessionExpiresAt: null,
          memberLocalDirRoot: null,
          userDrive: null,
          userDriveDeniedByProfile: "",
        }}
      />
    </MemoryRouter>,
  );
}

// Every background poll in the console keeps its last-good data on failure, so
// this banner is the ONLY thing separating a quiet fleet from a dead daemon —
// and the readiness chip must not report the outage as an unfinished setup.
// AppShell no longer polls setup/status itself (that duplicated App.tsx's own
// poll of the same expensive endpoint) — it takes confinementClasses as a
// prop and keeps the last value it was given when a re-render passes
// undefined (App.tsx's convention for "no fresh data this tick").
describe("AppShell (control plane unreachable)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShell(unreachable: boolean, confinementClasses?: ConfinementClass[]) {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));
    return render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
            unreachable={unreachable}
            lastOkAt={null}
            confinementClasses={confinementClasses}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  it("stays silent while the daemon answers", () => {
    renderShell(false);
    expect(screen.queryByText(/Control plane unreachable/)).toBeNull();
  });

  it("with nothing real to show yet, reads the same honest 'No barrier' a genuinely bare host would", () => {
    renderShell(true);
    expect(screen.getByText(/Control plane unreachable/)).toBeInTheDocument();
    expect(screen.getByText("No barrier")).toBeInTheDocument();
  });

  it("banners the outage and leaves the barrier chip at its last-known state", () => {
    const { rerender } = renderShell(false, ["CC1"]);
    expect(screen.queryByText("No barrier")).toBeNull();
    // App.tsx passes confinementClasses=undefined once its own probe reports
    // unreachable — the chip must not repaint from that absence.
    rerender(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
            unreachable={true}
            lastOkAt={null}
            confinementClasses={undefined}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
    expect(screen.getByText(/Control plane unreachable/)).toBeInTheDocument();
    expect(screen.queryByText("No barrier")).toBeNull();
  });
});

// W31-S1-7: the SSO session dies outright at its expiry with no refresh —
// this is the warning that never existed, pinned against /me's
// session_expires_at (an admin-token/local session, absent here, must never
// warn: it has nothing to expire).
describe("AppShell — session-expiry warning (W31-S1-7)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderWithMe(sessionExpiresAt: string | undefined) {
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
            principal: "cj@example.test",
            method: "sso",
            operator: true,
            role: "admin",
            email: "cj@example.test",
            session_expires_at: sessionExpiresAt,
          }),
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({}) });
    });
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    return render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  it("warns and offers a re-auth link when the session is about to die", async () => {
    renderWithMe(new Date(Date.now() + 2 * 60 * 1000).toISOString());
    expect(await screen.findByText(/session is expiring soon/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /sign in again/i })).toHaveAttribute("href", "/auth/login");
  });

  it("stays silent while the session has plenty of time left", async () => {
    renderWithMe(new Date(Date.now() + 60 * 60 * 1000).toISOString());
    await screen.findByText("No barrier"); // let /me resolve
    expect(screen.queryByText(/session is expiring soon/i)).toBeNull();
  });

  it("never warns for a session-less caller (admin token / local mode)", async () => {
    renderWithMe(undefined);
    await screen.findByText("No barrier");
    expect(screen.queryByText(/session is expiring soon/i)).toBeNull();
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

// Stage-1: the top bar's permanent barrier chip reads the strongest tier off
// the confinementClasses prop — App.tsx's OWN setup-status poll, never a
// second, disagreeing poll of the same expensive endpoint. The
// unreachable-daemon case (no confinement_classes to show) is covered above
// ("leaves the barrier chip at its last-known state"); this pins the
// positive case.
describe("AppShell — top bar barrier chip (stage-1)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the strongest available confinement tier once setup status resolves", () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
            confinementClasses={["CC1", "CC2"]}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
    // CC1+CC2 available => the strongest is Wall (CC2), never the raw CC2 code.
    expect(screen.getByText("Wall")).toBeInTheDocument();
    expect(screen.queryByText("No barrier")).toBeNull();
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
    expect(accountTrigger).toHaveClass("focus-visible:ring-ring");
  });
});

// Focus mode (design board 2c): the run cockpit can ask the shell to get out of
// the way. The shell's half of that contract — and, just as load-bearing, that
// it is OPT-IN: every other screen renders the header and sidebar exactly as
// before. Driven through the real context rather than a prop, because that is
// how the canvas reaches it.
describe("AppShell — focus mode hides the shell's own chrome", () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderShellWithRoute(child: React.ReactNode) {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    render(
      <MemoryRouter initialEntries={["/x"]}>
        <ThemeProvider>
          <Routes>
            <Route element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
              <Route path="/x" element={child} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  function FocusingScreen() {
    const { setFocus } = useFocusMode();
    React.useEffect(() => setFocus(true), [setFocus]);
    return <p>the cockpit</p>;
  }

  it("renders header and sidebar normally for a screen that never asks", async () => {
    renderShellWithRoute(<p>an ordinary screen</p>);
    expect(await screen.findByText("an ordinary screen")).toBeInTheDocument();
    expect(screen.getByRole("banner")).toBeInTheDocument();
    expect(screen.getByRole("complementary")).toBeInTheDocument();
  });

  it("drops both once a screen sets focus", async () => {
    renderShellWithRoute(<FocusingScreen />);
    await waitFor(() => expect(screen.queryByRole("banner")).toBeNull());
    expect(screen.queryByRole("complementary")).toBeNull();
    // The screen itself is untouched — only the shell's chrome went away.
    expect(screen.getByText("the cockpit")).toBeInTheDocument();
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
    for (const label of [
      "Runs",
      "Approvals",
      "Workspaces",
      "Policies",
      "Governance",
      "Permissions",
      "Secrets",
      "Audit",
      "Recordings",
    ]) {
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

// B3: member nav is Runs · Approvals · Workspaces, nothing else — no Policies/
// Permissions/Secrets/Audit/Recordings. Hiding is cosmetic (the server is the
// real boundary); this pins the UI half of that contract. Demos/Integrations
// left the sidebar entirely (stage-1 flatten) — Demos moved to the account
// menu (TopBar), which since Phase 5 hides it for members (its own describe
// block below) — routes.go still has no server-side gate on it at all.
describe("SidebarNav (member role — B3)", () => {
  // Workspaces joined the member set (mock M6): a member launches runs AGAINST
  // workspaces and could previously only glimpse them inside the New run
  // picker.
  it("shows Runs, Approvals and Workspaces — admin-only items are absent", async () => {
    const user = userEvent.setup();
    renderMobileNav("member");
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));

    for (const label of ["Runs", "Approvals", "Workspaces"]) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
    // Governance joins this list in 0.7: MEMBER_NAV_PATHS is unchanged, a
    // member never sees the item, and there is no member governance route.
    for (const label of ["Policies", "Governance", "Permissions", "Secrets", "Audit", "Recordings"]) {
      expect(screen.queryByRole("link", { name: new RegExp(`^${label}`) })).toBeNull();
    }
  });

  // Recordings is back on the admin sidebar (mock M6) after Audit: the route
  // and screen already existed and were reachable only by deep link, which
  // made the evidence trail undiscoverable.
  it("admin nav carries every item, with Recordings last — after Audit", async () => {
    const user = userEvent.setup();
    renderMobileNav("admin");
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));

    // Governance sits BETWEEN Policies and Permissions (mock Q1) — the order is
    // the contract this asserts, not an accident of the array.
    const labels = [
      "Runs",
      "Approvals",
      "Workspaces",
      "Policies",
      "Governance",
      "Permissions",
      "Secrets",
      "Audit",
      "Recordings",
    ];
    for (const label of labels) {
      expect(screen.getByRole("link", { name: new RegExp(`^${label}`) })).toBeInTheDocument();
    }
    // Order is part of the contract, not an accident of the array.
    const rendered = screen
      .getAllByRole("link")
      .map((el) => el.textContent ?? "")
      .filter((t) => labels.some((l) => t.startsWith(l)));
    expect(rendered.map((t) => labels.find((l) => t.startsWith(l)))).toEqual(labels);
  });
});

// Phase 5: the account-menu Demos entry (TopBar, not SidebarNav — the
// describe block above only drives the sidebar) is meaningless on a member's
// own Getting Started, which has no /setup?step= deep link at all.
function renderTopBar(role: Role) {
  return render(
    <MemoryRouter>
      <ThemeProvider>
        <TopBar
          onSignOut={() => {}}
          meta={{
            trustDomain: "example.test",
            identityProvider: "spiffe",
            principal: "u@example.test",
            method: "sso",
            resolved: true,
            operator: role === "admin",
            securityOperator: role !== "member",
            role,
            sessionExpiresAt: null,
            memberLocalDirRoot: null,
            userDrive: null,
            userDriveDeniedByProfile: "",
          }}
          pendingApprovals={0}
          attentionCount={0}
          confinementClasses={[]}
          onNewRun={() => {}}
        />
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("TopBar — account-menu Demos entry (Phase 5)", () => {
  it("member: no Demos item", async () => {
    const user = userEvent.setup();
    renderTopBar("member");
    await user.click(screen.getAllByRole("button").at(-1)!);
    const menu = screen.getByRole("menu");
    expect(within(menu).queryByText("Demos")).toBeNull();
  });

  it("admin: Demos item present", async () => {
    const user = userEvent.setup();
    renderTopBar("admin");
    await user.click(screen.getAllByRole("button").at(-1)!);
    const menu = screen.getByRole("menu");
    expect(within(menu).getByText("Demos")).toBeInTheDocument();
  });
});

// Phase 5: the landing gate (App.tsx's FirstRunLanding) waits on the shell's
// roleResolved signal. It must mean "the /me fetch SETTLED", never "method is
// non-empty" — a failed /me leaves method "" for good, and a signal derived
// from it would strand "/" on a spinner forever. Here fetch rejects outright
// (whoami → null), so the signal has to flip on the failure path too.
describe("AppShell (roleResolved after a failed /me)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function Probe() {
    return <span data-testid="probe">{useRoleResolved() ? "resolved" : "pending"}</span>;
  }

  it("flips to resolved once /me settles, even when it fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));
    render(
      <MemoryRouter>
        <ThemeProvider>
          <Routes>
            <Route
              element={
                <AppShell
                  pendingApprovals={0}
                  attentionCount={0}
                  onSignOut={() => {}}
                  unreachable={true}
                  lastOkAt={null}
                  confinementClasses={[]}
                />
              }
            >
              <Route index element={<Probe />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
    // Negative control: the first paint is pending — the signal is not a
    // constant true (which would defeat the member/admin race the gate closes).
    expect(screen.getByTestId("probe")).toHaveTextContent("pending");
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("resolved"));
  });
});
