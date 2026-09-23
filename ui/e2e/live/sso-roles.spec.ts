/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE ROLE WALK — every Dex identity, signed in for real, on one chart render.
 *
 * Four renders, one file. scripts/kind-sso-walk.sh runs it TWICE after the AWS
 * SSO walk: `sso` (the chart with SSO + the admin token) and `sso-only`
 * (auth.ssoOnly=true, token removed). scripts/compose-sso-roles.sh runs it on
 * `mprime` (the desktop member-mode envelope) and `compose-sso` (the compose
 * --profile sso stack). WARDYN_LIVE_ROLES_RENDER names which one this
 * invocation runs against, and the first case refuses to go on if /healthz
 * disagrees — so every assertion below is on the render its title names.
 *
 * What it proves that the hermetic suite cannot: that a real Dex sign-in
 * DERIVES the role the chart's role map and operator allowlist say it should,
 * and that the console and the API then treat that session accordingly. The
 * route-by-route matrix across all shapes lives at the API level
 * (internal/api's TestSSOShapeRoleMatrix); this file samples it through a
 * browser: one super-admin route, one security route, one member route, and
 * the cross-member existence oracle on a workspace member@ creates.
 *
 * Nothing here touches AWS or launches a sandbox, and the one workspace it
 * creates is deleted before it ends.
 */

import { randomUUID } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import { GOVERNANCE } from "../../src/app/lib/governance-copy";
import { SIGNIN } from "../../src/app/lib/sign-in-copy";
import { ADMIN_EMAIL, ADMIN_TOKEN, MEMBER_EMAIL, dexSignIn, me } from "./helpers";

test.skip(
  process.env.WARDYN_TEST_K8S !== "1" && process.env.WARDYN_TEST_SSO_ROLES !== "1",
  "live role walk: run scripts/kind-sso-walk.sh (WARDYN_TEST_K8S=1) or scripts/compose-sso-roles.sh (WARDYN_TEST_SSO_ROLES=1)",
);
test.describe.configure({ mode: "serial" });

const RENDER = process.env.WARDYN_LIVE_ROLES_RENDER || "sso";
const SSO_ONLY = RENDER === "sso-only";
// Member mode (docs/DESKTOP.md) keeps the token as the MDM's process credential:
// it still works as a bearer, but the console never offers it to a human.
const TOKEN_LOGIN = RENDER === "sso" || RENDER === "compose-sso";
// m′'s default role is member, by design: an unmapped login is a member there
// and refused everywhere else.
const UNMAPPED_IS_MEMBER = RENDER === "mprime";
const DEX_PASSWORD = "password";

const ADMIN_NAV = ["Policies", GOVERNANCE.TITLE, "Permissions", "Secrets", "Audit", "Recordings"];

type Want = { role: string; operator: boolean; security: boolean };
const ADMIN: Want = { role: "admin", operator: true, security: true };
const SEC: Want = { role: "security_admin", operator: false, security: true };
const MEMBER: Want = { role: "member", operator: false, security: false };

// deploy/kind/sso/dex.yaml's cast; oidc's TestShippedShapeRoleDerivation holds
// the same table against the shipped role map.
const CAST: Array<[string, Want]> = [
  [ADMIN_EMAIL, ADMIN],
  ["operator@wardyn.local", ADMIN],
  ["secadmin@wardyn.local", SEC],
  [MEMBER_EMAIL, MEMBER],
  ["member2@wardyn.local", MEMBER],
];

// The handler answered on its own merits: not refused, not hidden.
const REACHED = (status: number) => ![401, 403, 404].includes(status);

function ssoControl(page: Page) {
  return page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first();
}

async function api(page: Page, method: string, path: string, body?: unknown): Promise<{ status: number; text: string }> {
  return page.evaluate(
    async ([m, p, b]) => {
      const r = await fetch(p as string, {
        method: m as string,
        credentials: "include",
        headers: b === undefined ? {} : { "Content-Type": "application/json" },
        body: b === undefined ? undefined : JSON.stringify(b),
      });
      return { status: r.status, text: await r.text() };
    },
    [method, path, body] as const,
  );
}

test(`[${RENDER}] the render is the one this invocation claims, and the sign-in screen matches it`, async ({ page, request }) => {
  const hz = await (await request.get("/healthz")).json();
  expect(hz.sso, "/healthz: SSO is not configured on this render").toBe(true);
  expect(hz.sso_only, `/healthz.sso_only does not match render ${RENDER}`).toBe(SSO_ONLY);
  expect(hz.token_login, `/healthz.token_login on render ${RENDER}`).toBe(TOKEN_LOGIN);

  await page.goto("/");
  await expect(ssoControl(page)).toBeVisible({ timeout: 60_000 });
  await expect(page.locator("#token")).toHaveCount(TOKEN_LOGIN ? 1 : 0);
});

test(`[${RENDER}] the admin-token principal exists only where a token is configured`, async ({ request }) => {
  expect(ADMIN_TOKEN, "WARDYN_LIVE_ADMIN_TOKEN is unset — run this through scripts/kind-sso-walk.sh").not.toBe("");
  const headers = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  const res = await request.get("/api/v1/me", { headers });
  if (SSO_ONLY) {
    expect(res.status(), "sso-only: the walk's former admin token still opens the API").toBe(401);
    return;
  }
  expect(res.status()).toBe(200);
  expect(await res.json()).toMatchObject({ method: "token", role: "admin", operator: true });
  expect((await request.get("/api/v1/site-config", { headers })).status()).toBe(200);
});

for (const [email, want] of CAST) {
  test(`[${RENDER}] ${email} signs in as ${want.role}; console and API agree`, async ({ page }) => {
    await dexSignIn(page, email);
    const who = await me(page);
    expect(who.email).toBe(email);
    expect(who).toMatchObject({ role: want.role, operator: want.operator, security_operator: want.security });

    // The console: a member is offered no admin screen; every other tier gets the
    // full nav (app-shell.tsx's navItemsForRole) and each screen gates itself.
    for (const label of ADMIN_NAV) {
      await expect(page.getByRole("link", { name: new RegExp(`^${label}`) }), label).toHaveCount(want.role === "member" ? 0 : 1);
    }

    // The API: a super-admin route, a security route, a member route.
    expect((await api(page, "GET", "/api/v1/site-config")).status, "GET /site-config (super admin)").toBe(want.operator ? 200 : 403);
    expect((await api(page, "GET", "/api/v1/permissions")).status, "GET /permissions (security tier)").toBe(want.security ? 200 : 403);
    expect((await api(page, "GET", "/api/v1/runs")).status, "GET /runs (every role)").toBe(200);
  });
}

test(`[${RENDER}] a login that matches no role is ${UNMAPPED_IS_MEMBER ? "a member" : "refused"}`, async ({ page }) => {
  if (UNMAPPED_IS_MEMBER) {
    await dexSignIn(page, "stranger@wardyn.local");
    expect(await me(page)).toMatchObject({ role: "member", operator: false, security_operator: false });
    return;
  }
  await page.goto("/");
  await ssoControl(page).click();
  await page.locator('input[type="password"]').waitFor({ timeout: 60_000 });
  await page.locator('input[type="text"], input[name="login"]').first().fill("stranger@wardyn.local");
  await page.locator('input[type="password"]').fill(DEX_PASSWORD);
  await page.getByRole("button", { name: /log ?in/i }).click();
  await expect(page.getByText(SIGNIN.NO_ROLE)).toBeVisible({ timeout: 60_000 });
  expect((await api(page, "GET", "/api/v1/me")).status).toBe(401);
});

test(`[${RENDER}] one member cannot see or touch another member's workspace — the existence-oracle 404`, async ({
  browser,
  baseURL,
}) => {
  const as = async (email: string) => {
    const page = await (await browser.newContext({ baseURL })).newPage();
    await dexSignIn(page, email);
    return page;
  };
  const owner = await as(MEMBER_EMAIL);
  const created = await api(owner, "POST", "/api/v1/workspaces", { name: `roles-${RENDER}-${Date.now()}` });
  expect(created.status, created.text).toBe(201);
  const id = JSON.parse(created.text).id as string;
  const own = `/api/v1/workspaces/${id}/env-as-code`;
  expect(REACHED((await api(owner, "GET", own)).status), "the owner reads their own workspace").toBe(true);

  const other = await as("member2@wardyn.local");
  const missing = await api(other, "GET", `/api/v1/workspaces/${randomUUID()}/env-as-code`);
  const foreign = await api(other, "GET", own);
  expect(foreign.status, "member2 on member's workspace").toBe(404);
  expect(foreign.text, "the foreign 404 must be byte-identical to a missing one").toBe(missing.text);
  expect((await api(other, "DELETE", `/api/v1/workspaces/${id}`)).status, "member2 deleting member's workspace").toBe(404);

  // An admin reaches it (and cleans up).
  const admin = await as(ADMIN_EMAIL);
  expect(REACHED((await api(admin, "GET", own)).status), "an admin reaches a member's workspace").toBe(true);
  expect((await api(admin, "DELETE", `/api/v1/workspaces/${id}`)).status).toBeLessThan(300);
});
