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
import { describe, it, expect, vi, afterEach, type Mock } from "vitest";
import { act, render, screen, cleanup, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import App, { FirstRunLanding } from "./App";
import { roleCanReach } from "./components/wardyn/reauth-layer";
import { SESSION_ENDED_REASON } from "./lib/api/core";
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
  fetch: Mock;
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

describe("App — the sign-in screen's notice (H1, #483)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    sessionStorage.clear();
    cleanup();
  });

  // A visitor who never had a session sees the gate with nothing to explain.
  it("a cold-mount 401 with no stored token (a first visit) shows no notice", async () => {
    vi.stubGlobal("fetch", mockFetch({ probeUnauthed: true }).fetch);
    renderApp();
    await screen.findByText("Admin token", { exact: true });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(SESSION_ENDED_REASON)).toBeNull();
  });

  // #483: a stored token refused on load is a session this browser held — the
  // notice is an amber warning (role=status), never the error box.
  it("a cold-mount 401 for a stored token shows the signed-out warning, not an error", async () => {
    sessionStorage.setItem("wardyn_admin_token", "expired-token");
    vi.stubGlobal("fetch", mockFetch({ probeUnauthed: true }).fetch);
    renderApp();
    await screen.findByText("Admin token", { exact: true });
    expect(await screen.findByText(SESSION_ENDED_REASON)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

// The reauth dialog itself (#483) is pinned in App.reauth.test.tsx; the path a
// different person reloads to is decided by roleCanReach, pinned directly here.

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

  // M3: MEMBER_REACHABLE_PREFIXES must be the member REACHABLE set, not
  // merely the member NAV set — /secrets (self-service WRITE/DELETE since
  // migration 0050) and /settings + /ssh-keys (rendered in the account menu
  // for every role) have no sidebar entry but ARE reachable, or a member's
  // own mid-session 401 on any of the three would read as "no longer yours"
  // and send them to /runs instead of carrying on.
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

// 0.7.6 — /setup/status is now POLLED (the model-access door rides it), so the
// three things a poll can get wrong are pinned here: how often it asks, what a
// failure does to the last good answer, and what happens to an answer that
// arrives for a person who has since signed out.
describe("App — the setup-status poll behind the model-access door", () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    window.history.pushState({}, "", "/");
    cleanup();
  });

  /** mockFetch, with /setup/status counted and — once `hold` is armed — held
   *  open, so a tick that lands mid-flight can be observed being dropped. */
  function statusFetch(): {
    fetch: Mock;
    calls: () => number;
    hold: () => { resolve: (v: Response) => void };
    fail: (on: boolean) => void;
  } {
    const base = mockFetch({}).fetch;
    let count = 0;
    let held: { promise: Promise<Response>; resolve: (v: Response) => void } | null = null;
    let failing = false;
    const fetch = vi.fn((url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      if (u.includes("/setup/status")) {
        count += 1;
        if (held) return held.promise;
        if (failing) return Promise.reject(new Error("control plane unreachable"));
      }
      return base(url, init);
    });
    return {
      fetch,
      calls: () => count,
      hold: () => {
        held = deferred<Response>();
        return held;
      },
      fail: (on: boolean) => {
        failing = on;
      },
    };
  }

  it("asks once at sign-in, again on the interval and on returning to the tab — and never twice at once", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const s = statusFetch();
    vi.stubGlobal("fetch", s.fetch);
    renderApp();
    await screen.findByText("runs screen stub");
    expect(s.calls()).toBe(1);

    // The interval (MODEL_ACCESS_POLL_MS, 5 min).
    await act(async () => {
      vi.advanceTimersByTime(300_000);
    });
    expect(s.calls()).toBe(2);

    // Returning to the tab refreshes NOW rather than up to five minutes later.
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(s.calls()).toBe(3);

    // …and a tick that lands while a read is still in flight is DROPPED, not
    // queued: usePoll's guard is promise-based, which only works because
    // refreshSetupStatus RETURNS its chain.
    const held = s.hold();
    await act(async () => {
      vi.advanceTimersByTime(300_000);
    });
    expect(s.calls()).toBe(4);
    await act(async () => {
      vi.advanceTimersByTime(300_000);
      vi.advanceTimersByTime(300_000);
    });
    expect(s.calls()).toBe(4);
    held.resolve(jsonResponse(200, SETUP_STATUS_READY));
  });

  it("keeps the last known answer when a refresh fails", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const s = statusFetch();
    vi.stubGlobal("fetch", s.fetch);
    renderApp();
    await screen.findByText("runs screen stub");

    s.fail(true);
    await act(async () => {
      vi.advanceTimersByTime(300_000);
    });
    expect(s.calls()).toBe(2);
    // Still on the screen the last good status routed to — a failed probe must
    // never trap the console behind an empty snapshot.
    expect(screen.getByText("runs screen stub")).toBeInTheDocument();
  });

  it("drops the previous person's snapshot on sign-out, and ignores a read that lands after it", async () => {
    // The mount read is held open, so nothing has been stored yet when the
    // person signs out; it then answers for someone who is no longer here.
    const landingRead = deferred<Response>();
    const secondRead = deferred<Response>();
    let statusCalls = 0;
    const base = mockFetch({}).fetch;
    const fetchMock = vi.fn((url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      if (u.includes("/setup/status")) {
        statusCalls += 1;
        return statusCalls === 1 ? landingRead.promise : secondRead.promise;
      }
      return base(url, init);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderApp();

    // Signed in, landing still waiting on its status read — sign out.
    const user = userEvent.setup();
    const header = await screen.findByRole("banner");
    await waitFor(() => expect(within(header).getByText("cj")).toBeInTheDocument());
    const headerButtons = within(header).getAllByRole("button");
    await user.click(headerButtons[headerButtons.length - 1]);
    await user.click(within(await screen.findByRole("menu")).getByText("Sign out"));
    await screen.findByText("Admin token", { exact: true });

    // The stale completion: the first person's snapshot arrives after they are
    // gone. It must not be stored.
    await act(async () => {
      landingRead.resolve(jsonResponse(200, SETUP_STATUS_READY));
    });

    await user.type(screen.getByLabelText("Admin token"), "any-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    // The next person's landing decision waits for THEIR OWN read: with the
    // stale body stored, the landing would have routed off it already.
    await waitFor(() => expect(statusCalls).toBe(2));
    expect(screen.queryByText("runs screen stub")).toBeNull();

    secondRead.resolve(jsonResponse(200, SETUP_STATUS_READY));
    await screen.findByText("runs screen stub");
  });
});
