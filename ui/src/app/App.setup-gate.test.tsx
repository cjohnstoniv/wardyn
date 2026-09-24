/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #469: a gated install reached the funnel and then landed on Runs. The gate's
// redirect is applied inside a router transition, and the shell's status can be
// replaced by an unreachable read's made-up payload — either one used to hand
// the "/" landing a reason to leave the funnel.
import * as React from "react";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { act, render, screen, cleanup, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BrowserRouter, Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import App, { FirstRunLanding, RequireSetup } from "./App";
import { RoleProvider } from "./components/wardyn/operator-context";
import { resetGateForTests } from "./components/screens/setup/setup-gate";
import { baseStatus } from "./lib/test-fixtures";
import type { SetupStatus } from "./lib/types";

vi.mock("./components/screens/runs", () => ({
  RunsScreen: () => <div>runs screen stub</div>,
}));

// The funnel itself is out of scope — only whether the console stays in it.
// The key shows a NEW visit to the funnel apart from the one already painted.
vi.mock("./components/screens/onboarding/onboarding-screen", () => ({
  GettingStarted: () => (
    <div>
      setup funnel stub <span data-testid="funnel-key">{useLocation().key}</span>
      <Link to="/">home</Link>
      <Link to="/admin/runs">admin runs</Link>
    </div>
  ),
}));

const BLOCKING = { id: "runner", label: "Runner", status: "fail" as const, detail: "", blocking: true };

beforeEach(() => resetGateForTests());
afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

describe("RequireSetup — a re-render before the redirect lands (#469)", () => {
  // Bump stands in for App's own state updates (a badge or health poll
  // answering): its effect runs in the same commit as the gate's <Navigate>,
  // after the router has queued the route change as a transition, so React
  // re-renders the wrapper at the OLD location first.
  function Bump({ onBump }: { onBump: () => void }) {
    React.useEffect(onBump, []);
    return null;
  }
  function Harness({ status }: { status: SetupStatus }) {
    const [, rerender] = React.useState(0);
    return (
      <RoleProvider role="admin">
        <Routes>
          <Route element={<RequireSetup status={status} />}>
            <Route path="/" element={<FirstRunLanding status={status} />} />
            <Route path="/runs" element={<div>runs screen</div>} />
          </Route>
          <Route path="/admin/setup" element={<div>setup funnel</div>} />
        </Routes>
        <Bump onBump={() => rerender((n) => n + 1)} />
      </RoleProvider>
    );
  }

  it("a gated install lands in the funnel even when the shell re-renders mid-redirect", async () => {
    // has_runs:true is what made the stale render's FirstRunLanding pick Runs.
    const status = baseStatus({ has_runs: true, onboarding_complete: false, checks: [BLOCKING] });
    await act(async () => {
      render(
        <MemoryRouter initialEntries={["/"]}>
          <Harness status={status} />
        </MemoryRouter>,
      );
    });
    expect(screen.getByText("setup funnel")).toBeInTheDocument();
    expect(screen.queryByText("runs screen")).toBeNull();
  });
});

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as Response;
}
const GATED = {
  ready: false,
  has_runs: false,
  onboarding_complete: false,
  checks: [BLOCKING],
  auth: { mode: "token", local_loopback: false },
  runner: { driver: "docker", confinement_classes: ["CC1"] },
  providers: [],
  secrets: { present: [], github_app: false },
  age_key: { durable: true },
  platform: { os: "linux", wsl: false },
};
const ME_ADMIN = { principal: "cj", method: "token", operator: true, security_operator: true, role: "admin", email: "" };

/** Every /setup/status read after the first `good` ones answers 500.
 *  `browser` mounts over jsdom's real window.history, cold-loaded at "/". */
function mountApp(good: number, browser = false): () => number {
  let reads = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/setup/status")) {
        reads += 1;
        return Promise.resolve(reads <= good ? jsonResponse(200, GATED) : jsonResponse(500, {}));
      }
      if (u.includes("/healthz")) return Promise.resolve(jsonResponse(200, { status: "ok", sso: false }));
      if (u.includes("/readyz")) return Promise.resolve(jsonResponse(200, { status: "ok" }));
      if (u.includes("/me")) return Promise.resolve(jsonResponse(200, ME_ADMIN));
      if (u.includes("/approvals") || u.includes("/runs")) return Promise.resolve(jsonResponse(200, []));
      return Promise.resolve(jsonResponse(200, {}));
    }),
  );
  if (browser) {
    window.history.replaceState(null, "", "/");
    render(
      <BrowserRouter>
        <App />
      </BrowserRouter>,
    );
  } else {
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
  }
  return () => reads;
}

describe("App — an unreachable status read never replaces a real one (#469)", () => {
  it("a failed re-read keeps the gated status: '/' still lands in the funnel, not on Runs", async () => {
    const reads = mountApp(1);
    await screen.findByText("setup funnel stub");
    const firstVisit = screen.getByTestId("funnel-key").textContent;

    // The refocus re-read answers 500 — READY_FALLBACK, has_runs:false, no checks.
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(reads()).toBe(2));

    // "/" decides from the shell's status: the real one says this install has
    // never been onboarded, so it lands back in the funnel. The fallback would
    // have read "unreachable" and sent the operator to Runs.
    await userEvent.setup().click(screen.getByRole("link", { name: "home" }));
    await waitFor(() => {
      const again = screen.queryByTestId("funnel-key")?.textContent;
      expect(screen.queryByText("runs screen stub") ?? (again !== firstVisit ? again : null)).not.toBeNull();
    });
    expect(screen.queryByText("runs screen stub")).toBeNull();
    expect(screen.getByText("setup funnel stub")).toBeInTheDocument();
  });

  it("a FIRST read that fails still lands on Runs — nothing better is known, and the funnel must not trap", async () => {
    mountApp(0);
    await screen.findByText("runs screen stub");
    expect(screen.queryByText("setup funnel stub")).toBeNull();
  });
});

describe("App — once in the funnel, the gate stays spent for the load (#469 review)", () => {
  afterEach(() => window.history.replaceState(null, "", "/"));

  // The router keys every entry it did not push "default" — the cold load the
  // gate fired from, and a plain fragment link's entry alike. Leaving the
  // funnel and then taking the shell's skip link must not re-fire the gate.
  async function leaveTheFunnel(): Promise<void> {
    await screen.findByText("setup funnel stub");
    await userEvent.setup().click(screen.getByRole("link", { name: "admin runs" }));
    await screen.findByText("runs screen stub");
    expect(window.location.pathname).toBe("/admin/runs");
  }
  async function expectStillOnRuns(): Promise<void> {
    // Settle the router's transition before asserting nothing moved.
    await act(async () => {});
    expect(window.location.pathname).toBe("/admin/runs");
    expect(screen.getByText("runs screen stub")).toBeInTheDocument();
    expect(screen.queryByText("setup funnel stub")).toBeNull();
  }

  it("the skip link (a fragment anchor) keeps an operator who left the funnel where they are", async () => {
    mountApp(1, true);
    await leaveTheFunnel();
    await userEvent.setup().click(screen.getByRole("link", { name: /skip to main content/i }));
    await expectStillOnRuns();
  });

  it("an untagged history entry (pushState(null) + popstate) does not re-fire it either", async () => {
    mountApp(1, true);
    await leaveTheFunnel();
    await act(async () => {
      window.history.pushState(null, "", "/admin/runs#main-content");
      window.dispatchEvent(new PopStateEvent("popstate", { state: null }));
    });
    await expectStillOnRuns();
  });
});
