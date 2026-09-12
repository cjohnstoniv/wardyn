/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "./app-shell";
import {
  useOperator,
  useRole,
  useRoleResolved,
  useSecurityOperator,
} from "../wardyn/operator-context";
import { ThemeProvider } from "../wardyn/theme-provider";
import { SHELL } from "../wardyn/copy";

// V1-D3 — B1 gated the NAV and nothing else.
//
// navItemsForRole returned [] correctly and the banner + Retry worked, but every
// route stayed reachable by URL: the account menu's Settings link was
// unconditional, and RequireSetup answered a settled-but-unknown identity with
// <Outlet/>. So /settings painted the operator-only Model-provider
// Connect/Disconnect card plus the Providers card into admin /providers, and a
// bookmark or a reload on /permissions, /governance, /policies, /secrets or
// /audit painted the full admin screen — all off useMeta's fail-open seed
// (operator ?? true, role ?? "admin"), which is the seed B1 deliberately did NOT
// harden and which this fix deliberately still does not harden.
//
// The gate is at the ROUTE SHELL instead, the one place every route renders
// under: unknown identity ⇒ no route, banner + Retry, and no Settings link. The
// context defaults keep failing open for the ordinary not-settled-yet paint,
// which is the rationale B1 recorded.

const ADMIN_ROUTES = [
  "/settings",
  "/governance",
  "/permissions",
  "/audit",
] as const;

function stubMe(me: Record<string, unknown> | null) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.endsWith("/healthz")) {
        return Promise.resolve({
          ok: true,
          json: async () => ({
            trust_domain: "wardyn.local",
            identity_provider: "dex",
          }),
        });
      }
      if (u.endsWith("/api/v1/me")) {
        return me === null
          ? Promise.reject(new Error("connection refused"))
          : Promise.resolve({ ok: true, json: async () => me });
      }
      return Promise.resolve({ ok: true, json: async () => ({}) });
    }) as unknown as typeof fetch,
  );
}

// The screens are stood in for by markers: what is under test is whether the
// shell renders the route AT ALL, not what each admin screen draws.
function renderAt(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
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
            {ADMIN_ROUTES.map((p) => (
              <Route
                key={p}
                path={p}
                element={<p data-testid="screen">{`screen ${p}`}</p>}
              />
            ))}
            <Route
              path="/runs"
              element={<p data-testid="screen">screen /runs</p>}
            />
          </Route>
        </Routes>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

function accountTrigger() {
  const header = screen.getByRole("banner");
  const buttons = within(header).getAllByRole("button");
  return buttons[buttons.length - 1];
}

describe("AppShell — a settled-but-unknown identity gets no route (V1-D3)", () => {
  afterEach(() => vi.unstubAllGlobals());

  for (const path of ADMIN_ROUTES) {
    it(`renders the banner and NOT the screen at ${path}`, async () => {
      stubMe(null);
      renderAt(path);
      await waitFor(() =>
        expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
      );
      expect(screen.queryByTestId("screen")).toBeNull();
      // The Retry is the only thing on the page that does anything.
      expect(
        screen.getAllByRole("button", { name: SHELL.UNKNOWN_ACTION }).length,
      ).toBeGreaterThan(0);
    });
  }

  it("offers no Settings link in the account menu, and keeps Sign out", async () => {
    stubMe(null);
    renderAt("/runs");
    await waitFor(() =>
      expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
    );
    await userEvent.setup().click(accountTrigger());
    const menu = screen.getByRole("menu");
    expect(
      within(menu).queryByRole("menuitem", { name: /Settings/ }),
    ).toBeNull();
    // Sign out is the one control that still works for a human who cannot be
    // identified — hiding it would trap them on the banner.
    expect(within(menu).getByText("Sign out")).toBeInTheDocument();
  });

  it("a RESOLVED member still gets the member nav and the member route", async () => {
    stubMe({
      principal: "alice@corp.example",
      method: "sso",
      role: "member",
      operator: false,
      security_operator: false,
    });
    renderAt("/runs");
    await waitFor(() =>
      expect(screen.getByTestId("screen")).toHaveTextContent("screen /runs"),
    );
    expect(screen.queryByText(SHELL.UNKNOWN_BODY)).toBeNull();
    expect(
      screen.getAllByRole("link", { name: /^Runs/ }).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: /^Audit/ })).toBeNull();
  });

  it("a RESOLVED operator is unchanged: admin nav, admin route, Settings link", async () => {
    stubMe({
      principal: "root@wardyn.local",
      method: "token",
      role: "admin",
      operator: true,
      security_operator: true,
    });
    renderAt("/audit");
    await waitFor(() =>
      expect(screen.getByTestId("screen")).toHaveTextContent("screen /audit"),
    );
    expect(screen.queryByText(SHELL.UNKNOWN_BODY)).toBeNull();
    expect(
      screen.getAllByRole("link", { name: /^Audit/ }).length,
    ).toBeGreaterThan(0);

    await userEvent.setup().click(accountTrigger());
    const menu = screen.getByRole("menu");
    expect(
      within(menu).getByRole("menuitem", { name: /Settings/ }),
    ).toBeInTheDocument();
  });
});

// Phase 5: the landing gate (App.tsx's FirstRunLanding) waits on the shell's
// roleResolved signal. It must mean "the /me fetch SETTLED", never "method is
// non-empty" — a failed /me leaves method "" for good, and a signal derived
// from it would strand "/" on a spinner forever. Here fetch rejects outright
// (whoami → null), so the signal has to flip on the failure path too.
//
// B1 (0.7.2) RESTATES this describe rather than adding a sibling that would
// contradict it, because the two halves are one rule and the field report cost
// a customer hours by reading only the first:
//
//   1. roleResolved KEEPS meaning "settled, success or failure". Pointing it at
//      identityResolved instead — the first draft of this fix — would strand "/"
//      on RouteFallback forever, since nothing retried /me. The existing cases
//      below pin that, defaults and all.
//   2. …and "settled" is therefore NOT "answered". So the SIDEBAR is gated on
//      identityResolved: a settled-but-unknown identity renders NEITHER the
//      admin nav nor the member nav, plus one banner saying so and a Retry that
//      re-fires whoami(). Before this, a human the server had correctly DENIED
//      saw Policies / Governance / Permissions / Secrets / Audit off the
//      fail-open "admin" default — indistinguishable from an authz breach, on a
//      governance product, which is worse than a cosmetic bug.
//
// The tier defaults themselves stay fail-OPEN (case 2 below, R4/F119). That is
// the point: the fix is to stop DRAWING a nav from a guess, not to harden the
// guess into a different one.
describe("AppShell (roleResolved after a failed /me)", () => {
  afterEach(() => vi.unstubAllGlobals());

  function Probe() {
    return (
      <span data-testid="probe">
        {useRoleResolved() ? "resolved" : "pending"}
      </span>
    );
  }

  // R4/F119 — the TIER the failed /me leaves behind, which is the half this
  // describe never read. useMeta seeds operator/securityOperator/role with
  // `?? true` / `?? "admin"` and the comments around them call the DIRECTION
  // load-bearing ("an older daemon that never sends this field must fail OPEN
  // like every other identity signal here"; operator-context.tsx: 'Never
  // "harden" this default to false either'). Nothing asserted it: flipping all
  // three to `?? false` / `?? "member"` left the whole suite green, and the one
  // path where the defaults decide what a real human sees is exactly this one —
  // a 5xx, a dropped network, a pre-0.7 daemon. Fail-CLOSED here does not
  // protect anything (the server refuses every write regardless, requireOperator
  // is the enforcement point); it just hides the console from the admin who is
  // trying to find out what is wrong.
  function TierProbe() {
    return (
      <span data-testid="tier-probe">
        {JSON.stringify({
          operator: useOperator(),
          securityOperator: useSecurityOperator(),
          role: useRole(),
        })}
      </span>
    );
  }

  it("flips to resolved once /me settles, even when it fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("connection refused")),
    );
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
    // Negative control: the first paint is NOT settled — the signal is not a
    // constant true (which would defeat the member/admin race the gate closes).
    // Re-anchored off the route child (V1-D3): a settled-but-unknown identity
    // now paints no route at all, so the banner IS the settled signal.
    expect(screen.getByTestId("probe")).toHaveTextContent("pending");
    expect(screen.queryByText(SHELL.UNKNOWN_BODY)).toBeNull();
    await waitFor(() =>
      expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("probe")).toBeNull();
  });

  it("paints no route at all when /me never answers — the fail-open tier reaches nothing (V1-D3)", async () => {
    // R4/F119 pinned the TIER a failed /me leaves behind: useMeta seeds
    // operator/securityOperator/role with `?? true` / `?? "admin"`, and the
    // direction is load-bearing ("Never harden this default to false either") —
    // failing CLOSED there protects nothing, since requireOperator on the server
    // is the enforcement point, and it hides the console from the admin trying to
    // find out what is wrong. That rule STANDS, and the case below (a resolved
    // daemon whose /me omits the fields) is where it still reaches a screen.
    //
    // What V1-D3 changed is where the ignorance is answered. B1 gated the NAV
    // only, so the fail-open tier still painted: the account menu's Settings link
    // reached the operator-only Model-provider card in two clicks, and a bookmark
    // or reload on /permissions, /governance, /policies, /secrets, /audit painted
    // the full admin screen off that seed. The shell now renders NO route while
    // the identity is settled-but-unknown, so the defaults decide nothing a human
    // can see — which is why this case asserts the ABSENCE of the route child
    // rather than the tier it would have been handed.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("connection refused")),
    );
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
              <Route
                index
                element={
                  <>
                    <Probe />
                    <TierProbe />
                  </>
                }
              />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );

    // The fail-open seed is still what the context holds on the FIRST paint,
    // before the rejection lands — unhardened, exactly as F119 requires.
    expect(screen.getByTestId("tier-probe")).toHaveTextContent(
      JSON.stringify({ operator: true, securityOperator: true, role: "admin" }),
    );
    // …and once the /me is SETTLED and still unknown, the route is gone and the
    // banner is the page.
    await waitFor(() =>
      expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("tier-probe")).toBeNull();
    expect(screen.queryByTestId("probe")).toBeNull();
  });

  it("keeps the tier fail-OPEN for a daemon whose /me omits the fields entirely", async () => {
    // A pre-0.7 daemon: /me answers 200 with the identity keys it has always
    // sent and none of the three tier keys. Absent must read as OPEN, not as
    // "member" — the same rule, on the path that actually reaches the `??`.
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
        if (u.endsWith("/api/v1/me")) {
          return Promise.resolve({
            ok: true,
            json: async () => ({
              principal: "root@wardyn.local",
              method: "token",
            }),
          });
        }
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
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
                />
              }
            >
              <Route
                index
                element={
                  <>
                    <Probe />
                    <TierProbe />
                  </>
                }
              />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );

    // Wait for the real /me to settle, so this reads the RESOLVED value rather
    // than the seed it happens to equal.
    await waitFor(() =>
      expect(screen.getByTestId("probe")).toHaveTextContent("resolved"),
    );
    expect(screen.getByTestId("tier-probe")).toHaveTextContent(
      JSON.stringify({ operator: true, securityOperator: true, role: "admin" }),
    );
  });

  // ── B1, the half above pins the rule for ─────────────────────────────────
  // The tier stays fail-open (the three cases above) AND the sidebar stops
  // drawing anything from it. Both, or the fix is the one the round rejected.
  it("renders NEITHER nav and says why when /me never answers", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("connection refused")),
    );
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
                />
              }
            >
              <Route index element={<Probe />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
    );

    // Not the admin set — the fail-open default would have offered all of it…
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
    // …and not the member set either. "We don't know" is a THIRD answer, not a
    // quieter guess: showing the member nav would be just as unfounded.
    for (const label of ["Runs", "Approvals", "Workspaces"]) {
      expect(
        screen.queryByRole("link", { name: new RegExp(`^${label}`) }),
      ).toBeNull();
    }
    // One sentence in place of the guessed nav, and an action behind it.
    expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: SHELL.UNKNOWN_ACTION }),
    ).toBeInTheDocument();
  });

  it("Retry re-fires /me, and an answer restores the nav it earns", async () => {
    const user = userEvent.setup();
    // First /me rejects; the second answers as a MEMBER. Both halves matter:
    // the retry has to actually re-fetch (the effect ran once, on mount), and
    // what comes back has to drive the nav — proving the banner state was
    // ignorance and not a latch.
    let meCalls = 0;
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
        if (u.endsWith("/api/v1/me")) {
          meCalls++;
          if (meCalls === 1)
            return Promise.reject(new Error("connection refused"));
          return Promise.resolve({
            ok: true,
            json: async () => ({
              principal: "alice@corp.example",
              method: "sso",
              operator: false,
              security_operator: false,
              role: "member",
            }),
          });
        }
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
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
                />
              }
            >
              <Route index element={<Probe />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.getByText(SHELL.UNKNOWN_BODY)).toBeInTheDocument(),
    );
    expect(meCalls).toBe(1);

    await user.click(
      screen.getAllByRole("button", { name: SHELL.UNKNOWN_ACTION })[0],
    );

    await waitFor(() => expect(meCalls).toBe(2));
    // The banner is gone and the MEMBER nav — not the admin one it defaulted
    // to a moment ago — is what the answer produced.
    await waitFor(() =>
      expect(screen.queryByText(SHELL.UNKNOWN_BODY)).toBeNull(),
    );
    expect(
      screen.getAllByRole("link", { name: /^Runs/ }).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: /^Audit/ })).toBeNull();
  });
});
