/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// FirstRunLanding (Phase 5) now waits for BOTH the setup-status fetch AND the
// real role to resolve before it ever navigates — a member landed here under
// the role context's fail-open "admin" default would be routed by the wrong
// rule (App.tsx's own comment on the component explains why). This suite
// drives it directly with a stub RoleProvider rather than the whole App, since
// App's own auth/health polling has nothing to do with this decision.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import App, { FirstRunLanding, roleCanReach } from "./App";
import { RoleProvider, type Role } from "./components/wardyn/operator-context";
import { baseStatus } from "./lib/test-fixtures";
import type { SetupStatus } from "./lib/types";

// H1/H2/M2 — full App mount, real onUnauthorized wiring, mocked fetch. The
// board itself (RunsScreen) is stubbed: these cases only care about the
// auth/SignIn swap, not the screen behind it, and RunsScreen's own fetches
// (workspaces, demos…) are out of scope here.
vi.mock("./components/screens/runs", () => ({
  RunsScreen: () => <div>runs screen stub</div>,
}));

function renderLanding(role: Role, roleResolved: boolean, status: SetupStatus | null) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <RoleProvider role={role} roleResolved={roleResolved}>
        <Routes>
          <Route path="/" element={<FirstRunLanding status={status} />} />
          <Route path="/setup" element={<div>setup screen</div>} />
          <Route path="/runs" element={<div>runs screen</div>} />
        </Routes>
      </RoleProvider>
    </MemoryRouter>,
  );
}

describe("FirstRunLanding — waits for both status and role", () => {
  // Negative control: status is in, but the role hasn't resolved yet — must
  // not navigate at all (neither route's content renders).
  it("status resolved + role unresolved navigates nowhere", () => {
    renderLanding("member", false, baseStatus({ has_runs: false }));
    expect(screen.queryByText("setup screen")).toBeNull();
    expect(screen.queryByText("runs screen")).toBeNull();
  });

  it("navigates once role resolves — an unseen member lands on their own Getting Started", () => {
    renderLanding("member", true, baseStatus({ has_runs: false }));
    expect(screen.getByText("setup screen")).toBeInTheDocument();
  });

  it("admin is unaffected — the admin has_runs rule still applies once resolved", () => {
    renderLanding("admin", true, baseStatus({ has_runs: true }));
    expect(screen.getByText("runs screen")).toBeInTheDocument();
  });
});

// H1/H2/M2 — full App mount over a mocked fetch, driving the REAL wfetch ->
// onUnauthorized wiring (not a stub of core.ts) so the gating logic in
// App.tsx itself is what's under test.
function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as Response;
}

const ME_ADMIN = { principal: "cj", method: "token", operator: true, security_operator: true, role: "admin", email: "" };
const SETUP_STATUS_READY = {
  ready: true,
  has_runs: true,
  checks: [],
  auth: { mode: "token", local_loopback: false },
  runner: { driver: "docker", confinement_classes: ["CC1"] },
  providers: [],
  secrets: { present: [], github_app: false },
  age_key: { durable: true },
  platform: { os: "linux", wsl: false },
};

// A promise this test file resolves by hand, rather than a timer — under a
// full parallel `--maxWorkers` run, a fixed-delay setTimeout raced against
// React's own effect scheduling is exactly the kind of "usually fine, flaky
// under load" ordering bug this suite exists to catch elsewhere; a manually
// resolved promise has no timing dependency to get wrong.
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

// A single fetch mock covers every endpoint App.tsx + AppShell touch on
// mount: the probe (`/runs?limit=1`), `/healthz`/`/readyz` (raw fetch, no
// `/api/v1` prefix), `/setup/status`, `/me`, and the post-auth badge poll
// (`/runs?limit=1000`, `/approvals?...`). `on401` names exactly one path
// substring whose response is instead controlled by the returned `resolve401`
// — the caller decides WHEN that call settles (and with what), so the
// mid-session case can wait for the console to be genuinely authed and
// rendered before ever letting the 401 land. `probeUnauthed` (cold-mount
// case) needs no such control: nothing has rendered yet either way.
function mockFetch(opts: { probeUnauthed?: boolean; me?: typeof ME_ADMIN; on401?: string }): {
  fetch: ReturnType<typeof vi.fn>;
  resolve401: () => void;
} {
  const pending = opts.on401 ? deferred<Response>() : null;
  const fetch = vi.fn((url: RequestInfo | URL) => {
    const u = String(url);
    if (pending && opts.on401 && u.includes(opts.on401)) return pending.promise;
    if (u.includes("/runs?limit=1") && !u.includes("limit=1000")) {
      return Promise.resolve(jsonResponse(opts.probeUnauthed ? 401 : 200, []));
    }
    if (u.includes("/healthz")) return Promise.resolve(jsonResponse(200, { status: "ok", sso: false }));
    if (u.includes("/readyz")) return Promise.resolve(jsonResponse(200, { status: "ok" }));
    if (u.includes("/setup/status")) return Promise.resolve(jsonResponse(200, SETUP_STATUS_READY));
    if (u.includes("/me")) return Promise.resolve(jsonResponse(200, opts.me ?? ME_ADMIN));
    if (u.includes("/approvals")) return Promise.resolve(jsonResponse(200, []));
    if (u.includes("/runs")) return Promise.resolve(jsonResponse(200, []));
    return Promise.resolve(jsonResponse(200, {}));
  });
  return {
    fetch,
    resolve401: () => pending?.resolve(jsonResponse(401, { error: "unauthorized" })),
  };
}

function renderApp(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <App />
    </MemoryRouter>,
  );
}

describe("App — a 401 only carries a reason when the console WAS authed (H1)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    cleanup();
  });

  // The regression this pins: onUnauthorized used to fire unconditionally,
  // so the cold mount probe's OWN 401 (no session ever established this tab)
  // rendered "Your session ended…" to a visitor who never had one.
  it("a cold-mount 401 (never signed in) renders SignIn with no alert", async () => {
    vi.stubGlobal("fetch", mockFetch({ probeUnauthed: true }).fetch);
    renderApp();
    await screen.findByText("Admin token", { exact: true });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a mid-session 401 (was authed) renders the reason", async () => {
    const { fetch, resolve401 } = mockFetch({ on401: "limit=1000" });
    vi.stubGlobal("fetch", fetch);
    renderApp();
    // Authed and rendered FIRST — the badge poll's own 401 is a promise this
    // test hasn't resolved yet, so there is nothing to race: it cannot land
    // before this does, on any hardware.
    await screen.findByText("runs screen stub");
    resolve401();
    await screen.findByText("Admin token", { exact: true });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/session ended/i);
  });
});

// L4: onSignIn resolves identity BEFORE flipping `auth`, so the routed tree
// never mounts at the pre-401 URL for one commit before the bounce — proven
// by ORDER, not by DOM absence: while onSignIn's own `health.whoami()` call
// (the SECOND /me request, deferred here) is still pending, nothing that
// only fires once `auth === "authed"` (the badge poll's `limit=1000` reads)
// may have gone out yet.
describe("App — identity resolves before auth flips on re-auth (L4)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.history.pushState({}, "", "/");
    cleanup();
  });

  it("no post-auth fetch fires while the re-auth whoami() is still pending", async () => {
    // window.location.pathname is what safeReturnPath actually reads at
    // capture (core.ts) — MemoryRouter's own history never touches it, so
    // without this the captured path folds to the bare "/" the app happens
    // to boot at, safeReturnPath rewrites THAT to "/runs", and onSignIn's
    // `path === "/runs"` short-circuit skips whoami() entirely (a real path
    // is required to exercise the branch this pin is about).
    window.history.pushState({}, "", "/drives");

    const midSession401 = deferred<Response>();
    const reAuthWhoami = deferred<Response>();
    let meCalls = 0;
    let badgeCalls = 0;
    let reAuthClicked = false;
    let badgeCallsAfterClick = 0;
    const fetchMock = vi.fn((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/api/v1/me")) {
        meCalls++;
        // Call 1: AppShell's own mount-time /me (admin). Call 2+: the
        // onSignIn whoami() this test controls.
        return meCalls === 1 ? Promise.resolve(jsonResponse(200, ME_ADMIN)) : reAuthWhoami.promise;
      }
      if (u.includes("limit=1000")) {
        badgeCalls++;
        if (reAuthClicked) badgeCallsAfterClick++;
        // Call 1: the mid-session 401 this test drives on purpose. Every
        // later call (the SECOND badge poll, once re-authed) just succeeds.
        return badgeCalls === 1 ? midSession401.promise : Promise.resolve(jsonResponse(200, []));
      }
      if (u.includes("/runs?limit=1") && !u.includes("limit=1000")) return Promise.resolve(jsonResponse(200, []));
      if (u.includes("/healthz")) return Promise.resolve(jsonResponse(200, { status: "ok", sso: false }));
      if (u.includes("/readyz")) return Promise.resolve(jsonResponse(200, { status: "ok" }));
      if (u.includes("/setup/status")) return Promise.resolve(jsonResponse(200, SETUP_STATUS_READY));
      if (u.includes("/approvals")) return Promise.resolve(jsonResponse(200, []));
      if (u.includes("/runs")) return Promise.resolve(jsonResponse(200, []));
      return Promise.resolve(jsonResponse(200, {}));
    });
    vi.stubGlobal("fetch", fetchMock);
    renderApp();
    await screen.findByText("runs screen stub");

    midSession401.resolve(jsonResponse(401, { error: "unauthorized" }));
    await screen.findByText("Admin token", { exact: true });
    await screen.findByRole("alert");

    reAuthClicked = true;
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Admin token"), "any-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    // onSignIn is suspended on `await health.whoami()` right now (its
    // promise is still pending) — `setAuth("authed")` cannot have run yet,
    // so the badge poll it gates must not have fired either.
    expect(badgeCallsAfterClick).toBe(0);
    expect(screen.getByLabelText("Admin token")).toBeInTheDocument(); // still on the gate

    // Resolved as a MEMBER (fails roleCanReach("/drives", "member")) so the
    // fallback lands on the stubbed Runs screen rather than a real,
    // unmocked DrivesScreen — this test is about ORDER, not destination
    // (M2/M3 already cover the destination).
    reAuthWhoami.resolve(jsonResponse(200, { ...ME_ADMIN, role: "member", operator: false, security_operator: false }));
    await screen.findByText("runs screen stub");
    expect(badgeCallsAfterClick).toBeGreaterThan(0); // …and now it has.
  });
});

// H2/M2's actual path-restore behavior is proven end to end in
// e2e/auth.spec.ts (Playwright drives the REAL browser location — a
// MemoryRouter-based vitest mount can't: `safeReturnPath` reads
// `window.location.pathname`, which MemoryRouter's in-memory history never
// touches, so a vitest mount here would only prove the mock's own scripted
// path, not the capture/restore wiring). The pure `safeReturnPath`/
// `roleCanReach` functions are unit-pinned directly instead.

describe("roleCanReach — pure (M2)", () => {
  it("a member cannot reach an operator-only route", () => {
    expect(roleCanReach("/drives", "member")).toBe(false);
    expect(roleCanReach("/providers", "member")).toBe(false);
  });

  it("an admin can reach an operator-only route; a security admin cannot", () => {
    expect(roleCanReach("/drives", "admin")).toBe(true);
    expect(roleCanReach("/drives", "security_admin")).toBe(false);
  });

  it("a member reaches their own three-screen surface, including sub-routes", () => {
    expect(roleCanReach("/runs/abc-123", "member")).toBe(true);
    expect(roleCanReach("/workspaces", "member")).toBe(true);
  });

  // M3: MEMBER_REACHABLE_PREFIXES was the member NAV set, not the member
  // REACHABLE set — /secrets (self-service WRITE/DELETE since migration
  // 0050) and /settings + /ssh-keys (rendered in the account menu for every
  // role) have no sidebar entry but ARE reachable, so a member's own
  // mid-session 401 on any of the three used to bounce to /runs instead of
  // restoring.
  it("M3: a member reaches the three self-service routes with no sidebar entry", () => {
    expect(roleCanReach("/secrets", "member")).toBe(true);
    expect(roleCanReach("/settings", "member")).toBe(true);
    expect(roleCanReach("/ssh-keys", "member")).toBe(true);
  });

  // Negative control: a route with no special tier (neither member-scoped
  // nor operator-only) is reachable by any non-member role.
  it("neg: an ungated route is reachable by admin and security_admin alike", () => {
    expect(roleCanReach("/policies", "admin")).toBe(true);
    expect(roleCanReach("/policies", "security_admin")).toBe(true);
  });
});
