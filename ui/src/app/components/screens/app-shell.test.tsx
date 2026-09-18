/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell, MobileNav, SESSION_EXPIRY_COPY, TopBar, useFocusMode } from "./app-shell";
import { MODEL_ACCESS_BANNER } from "../wardyn/model-access-copy";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { useUserDrive, type Role } from "../wardyn/operator-context";
import { ThemeProvider } from "../wardyn/theme-provider";
import { baseMeDrive, baseStatus } from "../../lib/test-fixtures";

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
          email: "",
          name: "",
          method: "sso",
          resolved: true,
          identityResolved: true,
          operator: role === "admin",
          securityOperator: role !== "member",
          role,
          sessionExpiresAt: null,
          memberLocalDirRoot: null,
          userDrive: null,
          userDriveDeniedByProfile: "",
          userDriveUnavailable: "",
          memberMode: false,
          memberModeNoCredential: false,
          memberPreviewAvailable: false,
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
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("connection refused")),
    );
    return render(
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
  }

  it("stays silent while the daemon answers", () => {
    renderShell(false);
    expect(screen.queryByText(/Control plane unreachable/)).toBeNull();
  });

  it("banners the outage", () => {
    renderShell(true);
    expect(screen.getByText(/Control plane unreachable/)).toBeInTheDocument();
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
          json: async () => ({
            trust_domain: "wardyn.local",
            identity_provider: "embedded",
          }),
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
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
          />
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  it("warns and offers a re-auth link when the session is about to die", async () => {
    renderWithMe(new Date(Date.now() + 2 * 60 * 1000).toISOString());
    expect(
      await screen.findByText(/session is expiring soon/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /sign in again/i }),
    ).toHaveAttribute("href", "/auth/login");
  });

  it("stays silent while the session has plenty of time left", async () => {
    renderWithMe(new Date(Date.now() + 60 * 60 * 1000).toISOString());
    await screen.findByText("cj"); // let /me resolve
    expect(screen.queryByText(/session is expiring soon/i)).toBeNull();
  });

  it("never warns for a session-less caller (admin token / local mode)", async () => {
    renderWithMe(undefined);
    await screen.findByText("cj");
    expect(screen.queryByText(/session is expiring soon/i)).toBeNull();
  });

  // F3-F11: a session already past its expiry used to read "expiring soon"
  // forever (the predicate was one-sided) — the third state names it.
  it("F3-F11: a session already past its expiry reads 'has expired', not 'expiring soon'", async () => {
    renderWithMe(new Date(Date.now() - 60 * 1000).toISOString());
    expect(await screen.findByText(/session has expired/i)).toBeInTheDocument();
    expect(screen.queryByText(/expiring soon/i)).toBeNull();
  });

  // F3-F11: `new Date("not-a-date")` parses to an Invalid Date, not null —
  // every arithmetic read off it used to be NaN, disabling the banner
  // silently instead of failing loudly or falling back safely.
  it("F3-F11: an unparseable session_expires_at never warns (guarded, not NaN'd into silence)", async () => {
    renderWithMe("not-a-real-date");
    await screen.findByText("cj");
    expect(screen.queryByText(/session is expiring soon/i)).toBeNull();
    expect(screen.queryByText(/session has expired/i)).toBeNull();
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
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
          />
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
          json: async () => ({
            trust_domain: "wardyn.local",
            identity_provider: "embedded",
          }),
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
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
          />
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
    await waitFor(() =>
      expect(within(menu).getByText(/local mode/i)).toBeInTheDocument(),
    );
    expect(within(menu).queryByText("Sign out")).toBeNull();
  });

  it("still offers Sign out for a real SSO session", async () => {
    renderShellAs("sso");
    const menu = await openAccountMenu();
    await waitFor(() =>
      expect(within(menu).getByText("Sign out")).toBeInTheDocument(),
    );
  });
});

// ui-shellAuth-4: the account-menu trigger must render via the shared Button
// component (like every sibling header control) so keyboard focus gets the
// app's focus-visible ring instead of falling back to a raw <button>'s bare
// unthemed browser-default outline.
describe("AppShell — account-menu trigger uses the shared Button (ui-shellAuth-4)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("carries the shared Button's focus-visible ring classes", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("network down")),
    );
    render(
      <MemoryRouter>
        <ThemeProvider>
          <AppShell
            pendingApprovals={0}
            attentionCount={0}
            onSignOut={() => {}}
          />
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
    // A /me that ANSWERS. This describe is about focus mode, not identity, and a
    // rejecting fetch now also means "settled but unknown" — which paints no
    // route at all (V1-D3), so the screen under test would never mount.
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) =>
        String(url).endsWith("/api/v1/me")
          ? Promise.resolve({
              ok: true,
              json: async () => ({
                principal: "root@wardyn.local",
                method: "token",
                role: "admin",
              }),
            })
          : Promise.resolve({ ok: true, json: async () => ({}) }),
      ) as unknown as typeof fetch,
    );
    render(
      <MemoryRouter initialEntries={["/x"]}>
        <ThemeProvider>
          <Routes>
            <Route
              element={
                <AppShell
                  pendingApprovals={0}
                  attentionCount={0}
                  onSignOut={() => {}}
                />
              }
            >
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
    const trigger = screen.getByRole("button", {
      name: /open navigation menu/i,
    });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "Runs" })).toBeNull();
  });

  it("opening the drawer reveals every nav item and flips aria-expanded to true", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", {
      name: /open navigation menu/i,
    });
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
      expect(
        screen.getByRole("link", { name: new RegExp(`^${label}`) }),
      ).toBeInTheDocument();
    }
  });

  it("Escape closes the drawer and returns aria-expanded to false", async () => {
    const user = userEvent.setup();
    renderMobileNav();
    const trigger = screen.getByRole("button", {
      name: /open navigation menu/i,
    });
    await user.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");

    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(trigger).toHaveAttribute("aria-expanded", "false"),
    );
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
    await user.click(
      screen.getByRole("button", { name: /open navigation menu/i }),
    );

    for (const label of ["Runs", "Approvals", "Workspaces"]) {
      expect(
        screen.getByRole("link", { name: new RegExp(`^${label}`) }),
      ).toBeInTheDocument();
    }
    // Governance joins this list in 0.7: MEMBER_NAV_PATHS is unchanged, a
    // member never sees the item, and there is no member governance route.
    for (const label of [
      "Policies",
      "Governance",
      "Permissions",
      "Secrets",
      "Audit",
      "Recordings",
    ]) {
      expect(
        screen.queryByRole("link", { name: new RegExp(`^${label}`) }),
      ).toBeNull();
    }
  });

  // Recordings is back on the admin sidebar (mock M6) after Audit: the route
  // and screen already existed and were reachable only by deep link, which
  // made the evidence trail undiscoverable.
  it("admin nav carries every item, with Recordings last — after Audit", async () => {
    const user = userEvent.setup();
    renderMobileNav("admin");
    await user.click(
      screen.getByRole("button", { name: /open navigation menu/i }),
    );

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
      expect(
        screen.getByRole("link", { name: new RegExp(`^${label}`) }),
      ).toBeInTheDocument();
    }
    // Order is part of the contract, not an accident of the array.
    const rendered = screen
      .getAllByRole("link")
      .map((el) => el.textContent ?? "")
      .filter((t) => labels.some((l) => t.startsWith(l)));
    expect(rendered.map((t) => labels.find((l) => t.startsWith(l)))).toEqual(
      labels,
    );
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
            email: "",
            name: "",
            method: "sso",
            resolved: true,
            identityResolved: true,
            operator: role === "admin",
            securityOperator: role !== "member",
            role,
            sessionExpiresAt: null,
            memberLocalDirRoot: null,
            userDrive: null,
            userDriveDeniedByProfile: "",
            userDriveUnavailable: "",
            memberMode: false,
            memberModeNoCredential: false,
            memberPreviewAvailable: false,
          }}
          pendingApprovals={0}
          attentionCount={0}
          onNewRun={() => {}}
        />
      </ThemeProvider>
    </MemoryRouter>,
  );
}

// 0.7.3 F6: the Fence/NetworkPolicy chips are gone outright — no degraded
// chip, no replacement. Both were deployment-wide facts fixed at boot that
// never changed while the console was open; posture now lives on the setup
// Environment step alone (environment-step.tsx's k8sEgressRow/k8sClassesRow).
describe("TopBar — the header states no posture (0.7.3 F6)", () => {
  it("carries no NetworkPolicy or barrier chip", () => {
    renderTopBar("admin");
    const header = screen.getByRole("banner");
    expect(within(header).queryByText(/NetworkPolicy:/)).not.toBeInTheDocument();
    expect(
      within(header).queryByText(/^(Fence|Wall|Vault|No barrier)$/),
    ).not.toBeInTheDocument();
  });
});

describe("TopBar — account-menu Demos entry (Phase 5)", () => {
  it("member: no Demos item", async () => {
    const user = userEvent.setup();
    renderTopBar("member");
    await user.click(screen.getAllByRole("button").at(-1)!);
    const menu = screen.getByRole("menu");
    expect(within(menu).queryByText("Demos")).toBeNull();
  });

  // W6-3: the item deep-links to /setup?step=sealed-box, which only the SUPER
  // admin's SetupScreen honours. A security admin's /setup/status is redacted
  // on the same !isOperator predicate a member's is (internal/api/setup.go), so
  // they land where a member lands — a Getting Started that ignores ?step — and
  // the item is a dead invitation for them too. `role !== "admin"`, matching
  // setupGateActive and GettingStarted.
  it("security admin: no Demos item — the deep link is as dead for them as for a member", async () => {
    const user = userEvent.setup();
    renderTopBar("security_admin");
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

// 0.7 — the shell's ONE GET /me is what fills UserDriveContext: New Run and the
// member Getting Started page read the caller's drive off the context and issue
// no fetch of their own. That seam had only e2e coverage, and a component test
// cannot see it: workspace-card.test.tsx renders the block perfectly from props
// the shell might never actually hand it. Pinned here, at the one place the
// wire body becomes the context value.
describe("AppShell — /me's drive bits reach UserDriveContext", () => {
  afterEach(() => vi.unstubAllGlobals());

  function DriveProbe() {
    const { drive, deniedByProfile, unavailable } = useUserDrive();
    return (
      <>
        <span data-testid="drive-probe">
          {JSON.stringify({ drive, deniedByProfile })}
        </span>
        {/* R4/F091 — read as its OWN element rather than folded into the JSON
            above, so the three cases that predate the third key keep asserting
            the exact string they always did. */}
        <span data-testid="drive-unavailable-probe">{`[${unavailable}]`}</span>
      </>
    );
  }

  function renderShellWithMe(me: Record<string, unknown>) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz")) {
          return Promise.resolve({
            ok: true,
            json: async () => ({
              trust_domain: "wardyn.local",
              identity_provider: "embedded",
            }),
          });
        }
        if (u.endsWith("/api/v1/me"))
          return Promise.resolve({ ok: true, json: async () => me });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    return render(
      <MemoryRouter>
        <ThemeProvider>
          <Routes>
            <Route
              element={
                <AppShell
                  pendingApprovals={0}
                  attentionCount={0}
                  onSignOut={() => {}}
                />
              }
            >
              <Route index element={<DriveProbe />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  it("hands the allocation down verbatim once /me resolves", async () => {
    const drive = baseMeDrive();
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "member",
      user_drive: drive,
      user_drive_denied_by_profile: "",
    });

    // Negative control: the fail-CLOSED seed is what paints first, so a green
    // assertion below cannot be a constant.
    expect(screen.getByTestId("drive-probe")).toHaveTextContent(
      JSON.stringify({ drive: null, deniedByProfile: "" }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({ drive, deniedByProfile: "" }),
      ),
    );
  });

  it("carries the door as a SIBLING of the allocation, not a property of it", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "member",
      user_drive: null,
      user_drive_denied_by_profile: "Greenfield contractors",
    });

    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({
          drive: null,
          deniedByProfile: "Greenfield contractors",
        }),
      ),
    );
  });

  it("stays at no-allocation-and-no-door for a pre-0.7 daemon that sends neither field", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "member",
    });

    // Let /me land before reading the probe, so this is the RESOLVED value and
    // not the seed it happens to equal.
    await screen.findByText("alice");
    expect(screen.getByTestId("drive-probe")).toHaveTextContent(
      JSON.stringify({ drive: null, deniedByProfile: "" }),
    );
    // …and the third key reads as "nothing is wrong", not as a reason invented
    // out of an absent field.
    expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
      "[]",
    );
  });

  // R4/F091 — the THIRD key. /me suppresses the allocation for all four of
  // these (me.go:119-123), so `drive: null` is the SAME answer for every one of
  // them and the reason is the only thing that tells them apart. The shell
  // typed the key and read it nowhere, so the context handed every consumer
  // "you have no allocation" — the one remedy that is wrong in all four cases.
  // One case per token in the server's closed vocabulary
  // (user_drives_resolve.go:87-99): a token this test does not carry is a
  // token the console silently drops again.
  it.each([
    ["groups_snapshot_stale"],
    ["unmountable"],
    ["unavailable"],
    ["governance_unavailable"],
  ])(
    "carries %s through to UserDriveContext beside the suppressed allocation",
    async (reason) => {
      renderShellWithMe({
        principal: "alice@corp.example",
        method: "sso",
        operator: false,
        role: "member",
        user_drive: null,
        user_drive_denied_by_profile: "",
        user_drive_unavailable: reason,
      });

      // Negative control: the fail-closed seed paints first, so the assertion
      // below cannot be a constant.
      expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
        "[]",
      );
      await waitFor(() =>
        expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
          `[${reason}]`,
        ),
      );
      // The allocation stays null — the reason REPLACES the offer, it does not
      // ride beside one.
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({ drive: null, deniedByProfile: "" }),
      );
    },
  );

  it("keeps the reason and the door as SEPARATE bits — a denied door is not an outage", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "member",
      user_drive: null,
      user_drive_denied_by_profile: "Greenfield contractors",
      user_drive_unavailable: "",
    });

    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({
          drive: null,
          deniedByProfile: "Greenfield contractors",
        }),
      ),
    );
    expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
      "[]",
    );
  });
});

// 0.7.6, finding 2 — THE MODEL-ACCESS STRIP'S PLACE IN THE STACK.
//
// The band itself is pinned by model-access-banner.test.tsx; what only the
// shell can prove is WHERE it sits and where it is withheld. It renders LAST:
// a dying session, a dead control plane and an unknown identity are each the
// better explanation of what you are looking at, and are read first.
describe("AppShell (the model-access strip)", () => {
  const PER_USER_ROW = {
    id: "claude-code",
    display: "claude-code",
    has_gateway: false,
    has_login: true,
    enabled: true,
    mechanism: "bedrock_sso",
    credential_source: "per_user",
  };

  afterEach(() => vi.unstubAllGlobals());

  /** `me` as a promise lets a case hold /me open — the window where
   *  useOperator() is still the fail-open default. */
  function renderShellAt(path: string, me: Record<string, unknown> | Promise<Record<string, unknown>>) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => await me });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    return render(
      <MemoryRouter initialEntries={[path]}>
        <ThemeProvider>
          <ModelAccessProvider
            status={baseStatus({
              model_access: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
              harnesses: [PER_USER_ROW],
            })}
            onRefresh={() => {}}
          >
            <Routes>
              <Route
                path="*"
                element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}
              >
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  const MEMBER_WITH_DYING_SESSION = {
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "member",
    email: "alice@corp.example",
    // Inside SESSION_WARN_MS, so the session strip is on screen too.
    session_expires_at: new Date(Date.now() + 60_000).toISOString(),
  };

  it("renders BELOW the session-expiry banner", async () => {
    renderShellAt("/runs", MEMBER_WITH_DYING_SESSION);
    const session = await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    const model = await screen.findByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN);
    // DOCUMENT_POSITION_FOLLOWING: `model` comes after `session` in the DOM.
    expect(session.compareDocumentPosition(model) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is withheld on /setup — that page IS the door", async () => {
    renderShellAt("/setup", MEMBER_WITH_DYING_SESSION);
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it("is withheld on /settings for an OPERATOR, which already mounts the same pane", async () => {
    renderShellAt("/settings", { ...MEMBER_WITH_DYING_SESSION, operator: true, role: "admin" });
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    await waitFor(() => expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull());
  });

  // …and the member it does not: the Settings card's AWS button is
  // `disabled={!operator}` there, so hiding the strip would strand exactly the
  // person the refusal sentence sends to that page.
  it("stays for a MEMBER on /settings", async () => {
    renderShellAt("/settings", MEMBER_WITH_DYING_SESSION);
    expect(await screen.findByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
  });

  // S4: the live region is the SHELL's and it is EAGER. role="status" announces
  // CHANGES to a mounted region; a region that arrives together with its first
  // sentence — which is what a lazy chunk does — announces nothing.
  it("mounts its live region before the lazy strip, and with nothing to say", () => {
    // No dying session and a live credential: this wrapper is then the only
    // role=status region in the shell, and it is empty.
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me"))
          return Promise.resolve({ ok: true, json: async () => ({ principal: "a@b", role: "member", operator: false }) });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <ThemeProvider>
          <ModelAccessProvider status={baseStatus({ model_access: { state: "live" } })} onRefresh={() => {}}>
            <Routes>
              <Route path="*" element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
  });

  // S2: useOperator()'s fail-open default is TRUE, so until /me lands a member
  // under a dead shared credential would read the ADMIN's sentence and be
  // offered a sign-in the server refuses. The door says nothing until the
  // identity is known.
  it("says nothing about a shared-dead credential until /me answers, then the member's line", async () => {
    let answer: (me: Record<string, unknown>) => void = () => {};
    const pending = new Promise<Record<string, unknown>>((resolve) => (answer = resolve));
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => await pending });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    const SHARED_ACTION = "Your admin's model credential expired — ask them to reconnect it";
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <ThemeProvider>
          <ModelAccessProvider
            status={baseStatus({
              model_access: { state: "shared_expired", action: SHARED_ACTION },
              harnesses: [{ ...PER_USER_ROW, credential_source: "shared" }],
            })}
            onRefresh={() => {}}
          >
            <Routes>
              <Route path="*" element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );

    // While /me is in flight: no admin sentence, no button, no member line.
    // Two macrotasks first, so the strip's own lazy chunk has certainly
    // resolved and the silence below is the door's answer rather than a chunk
    // that had not arrived yet.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
    expect(screen.queryByText(SHARED_ACTION)).toBeNull();

    answer({ principal: "member@corp.example", role: "member", operator: false, security_operator: false });
    // …and once it lands, the member reads the server's instruction, with no
    // button: nobody but their admin can repair it.
    expect(await screen.findByText(SHARED_ACTION)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });
});
