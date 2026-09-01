/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { execFileSync } from "node:child_process";
import {
  test as base,
  expect,
  type Locator,
  type Page,
} from "@playwright/test";

// Shared Playwright fixtures for the Wardyn UI e2e suite. Specs run against the
// seeded test backend booted by scripts/e2e-backend.sh (real wardynd + Postgres +
// `none` runner). See playwright.config.ts for the base URL.

// The admin token the seeded backend is started with. The app stores it under
// localStorage["wardyn_admin_token"] and probes /api/v1/runs on mount to decide
// auth; injecting it before first navigation boots the app already signed in.
export const ADMIN_TOKEN = process.env.WARDYN_E2E_TOKEN || "wardyn-e2e-token";
const TOKEN_KEY = "wardyn_admin_token";

// `test` boots the app pre-authenticated so each spec lands directly in the
// console. Auth-flow specs that exercise sign-in/sign-out should import the raw
// `test` from "@playwright/test" instead and manage storage themselves.
export const test = base.extend({
  page: async ({ page }, use) => {
    await page.addInitScript(
      ([key, tok]) => {
        try {
          localStorage.setItem(key, tok);
        } catch {
          /* private mode — ignore */
        }
      },
      [TOKEN_KEY, ADMIN_TOKEN],
    );
    await use(page);
  },
});

export { expect };

// Sidebar labels (app-shell.tsx). Navigating by accessible name keeps specs
// resilient to markup churn and needs no shared test-ids. Runs is the first/
// default screen; the Fleet board was merged into Runs and no longer exists, so
// it is intentionally absent from this list.
export type NavLabel =
  | "Runs"
  | "Approvals"
  | "Workspaces"
  | "Policies"
  // 0.7 — sits between Policies and Permissions (app-shell.tsx's NAV_ITEMS), so
  // the three read as one narrowing sequence. Never in MEMBER_NAV_PATHS.
  | "Governance"
  | "Permissions"
  | "Secrets"
  | "Audit"
  | "Recordings";

// Sidebar entries are react-router <NavLink>s (role="link"), not <button>s.
// Their accessible name can carry trailing content beyond the label — Runs/
// Approvals a numeric badge ("Runs 2"), Getting started a StatusChip word
// ("Getting started Ready" / "…Checking…") — so match by prefix rather than
// an exact/suffix pattern.
export function sidebarLink(page: Page, label: NavLabel): Locator {
  return page.getByRole("link", { name: new RegExp(`^${label}`) });
}

// gotoConsole loads the app shell (pre-authed) and waits for the sidebar.
export async function gotoConsole(page: Page): Promise<void> {
  await page.goto("/");
  // "/" never stays "/": FirstRunLanding redirects to /runs or /setup once
  // status and role resolve. The sidebar mounts BEFORE that redirect fires, so
  // waiting on the sidebar alone returns with a Navigate still pending — and a
  // test that immediately pushes its own route can then have it clobbered by
  // the stale redirect (a race that widens under suite load; it cost a
  // member-console run at /runs/new). Console-ready means the landing settled.
  await page.waitForURL((u) => u.pathname !== "/");
  await expect(sidebarLink(page, "Runs")).toBeVisible();
}

// navTo clicks a sidebar entry and returns once the click is registered.
export async function navTo(page: Page, label: NavLabel): Promise<void> {
  await sidebarLink(page, label).click();
}

// navToRoute reaches a screen that is NOT in the sidebar (a run detail page, a
// workspace, /runs/new). Use navTo for anything the sidebar actually lists;
// reaching a sidebar entry this way would stop proving the link works.
//
// This is a CLIENT-SIDE navigation, not page.goto(), and that distinction is
// load-bearing: a full document load re-runs the app's auth probe, so any test
// that has already installed a failing route intercept would never mount the
// shell at all. React Router listens to popstate, so pushState + popstate is
// exactly what a <NavLink> click does.
export async function navToRoute(page: Page, path: string): Promise<void> {
  await page.evaluate((p) => {
    window.history.pushState({}, "", p);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, path);
}

// Member console (B3) — the seeded e2e backend authenticates every spec with a
// bare admin bearer token (ADMIN_TOKEN above), and isOperator
// (internal/api/http.go) reads "no session role to demote" for any caller with
// no OIDC human session — so a bearer-token caller is ALWAYS admin
// server-side; there is no way to reach a genuine member session through this
// harness without standing up OIDC. GET /api/v1/me's `role`/`operator` fields
// are spliced onto the REAL response (route.fetch() + patch + refulfill —
// same technique corp-network.spec.ts already uses) so principal/method stay
// genuine while the client believes it is signed in as a member. Everything
// else (runs list, secrets, approvals list) still comes from the real,
// unmodified, admin-scoped backend — specs using this prove the RENDER
// behavior a member role drives, not server-side ownership scoping itself
// (that's proven server-side: B2's own tests, and
// internal/api/runs_policy.go's handleListRuns / approvals.go's
// handleListApprovals creator-pager branches). Shared here (not declared in
// one spec file) because Playwright refuses a spec that imports another spec
// file (`--list` collects zero tests when it sees one).
export async function mockMemberRole(page: Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.role = "member";
    json.operator = false;
    json.security_operator = false;
    await route.fulfill({ response, json });
  });
}

// Security-admin console (0.7's third tier) — the same splice technique and the
// same harness ceiling as mockMemberRole above: the bearer-token backend is
// always admin server-side, so this proves the RENDER behavior the tier drives,
// never server-side authorization (that is pinned in Go — internal/api's
// isSecurityOperator tests and authz_test.go's route matrix).
//
// The three fields together ARE the tier's contract, and the asymmetry is the
// point: operator FALSE (the super-admin surfaces — secrets, LLM credential,
// setup, workspace writes, run attach — stay hidden) with security_operator
// TRUE (approvals, audit, permissions, governance profiles are offered). A
// fixture setting both true would prove nothing this tier does not already
// share with an admin.
export async function mockSecurityAdminRole(page: Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.role = "security_admin";
    json.operator = false;
    json.security_operator = true;
    await route.fulfill({ response, json });
  });
}

// Some specs seed state the API can't create (e.g. an approval — `POST
// /internal/approvals` needs a run-scoped token, not the admin one), so they talk
// to the backend's own Postgres via `docker exec`, mirroring the seeding scripts.
// Both drivers always set these env vars (scripts/run-ui-e2e.sh points at the e2e
// DB, scripts/screenshots.sh at its own wardyn_shots so a capture never clobbers
// it), so the defaults only matter when a spec is run by hand.
const PG_CONTAINER = process.env.WARDYN_E2E_PG_CONTAINER || "wardyn-test-pg";
const PG_DBNAME = process.env.WARDYN_E2E_PG_DBNAME || "wardyn_e2e";

// Run one SQL statement against that Postgres. -tA gives tuple-only, unaligned
// output so the caller can parse a single scalar trivially.
export function sql(statement: string): string {
  return execFileSync(
    "docker",
    [
      "exec",
      "-i",
      PG_CONTAINER,
      "psql",
      "-U",
      "wardyn",
      "-d",
      PG_DBNAME,
      "-tAc",
      statement,
    ],
    { encoding: "utf8" },
  ).trim();
}
