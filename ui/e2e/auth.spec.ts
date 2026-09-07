/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, type Page } from "@playwright/test";

// Auth / sign-in lane.
//
// This is the ONE spec that must NOT use the pre-authenticated fixture: it
// exercises the sign-in gate, wrong/right token submission, session
// persistence across reload, and sign-out (the /auth/logout HIGH fix). It
// therefore imports the RAW `test` from "@playwright/test" and drives
// localStorage["wardyn_admin_token"] itself.
//
// App boot (App.tsx) probes auth on mount: a stored admin token (or a live
// OIDC session cookie) lets it straight into the console; otherwise it renders
// the SignIn gate. sign-in.tsx persists the typed token, then probes a
// protected endpoint (probeAuth -> GET /api/v1/runs). A 401 clears the token
// and shows an error; a 200 calls onSignIn() and lands in the console.

const TOKEN_KEY = "wardyn_admin_token";
const GOOD_TOKEN = process.env.WARDYN_E2E_TOKEN || "wardyn-e2e-token";

// Clear the stored token BEFORE the app's first script runs so every test
// starts from a clean, unauthenticated slate regardless of prior state.
async function clearTokenInit(page: Page): Promise<void> {
  await page.addInitScript((key) => {
    try {
      localStorage.removeItem(key);
    } catch {
      /* private mode — ignore */
    }
  }, TOKEN_KEY);
}

// Seed a stored token for an origin, then boot the app already carrying it
// (mirrors a returning operator). We deliberately do NOT use addInitScript:
// an init script re-runs on every navigation, which would silently re-seed the
// token on a later reload and mask sign-out/persistence regressions. Instead we
// land on the origin once (the gate), write localStorage, then reload so the
// mount probe sees the stored token — a one-time seed that survives only as
// long as the app itself keeps it.
async function bootWithStoredToken(page: Page, token: string): Promise<void> {
  // Fresh context => localStorage starts empty; first load shows the gate.
  await page.goto("/");
  await page.evaluate(
    ([key, tok]) => localStorage.setItem(key, tok),
    [TOKEN_KEY, token],
  );
  await page.reload();
}

async function readToken(page: Page): Promise<string | null> {
  // Mirror getToken's `ssGet(key) ?? lsGet(key)` resolution (lib/api/core.ts): a
  // default sign-in ("Remember on this device" off) now persists to sessionStorage
  // and clears localStorage, so a localStorage-only read would miss it. The
  // returning-operator flows seed localStorage directly, which the fallback covers.
  return page.evaluate(
    (key) => sessionStorage.getItem(key) ?? localStorage.getItem(key),
    TOKEN_KEY,
  );
}

// The sign-in gate, identified by its "Admin token" field + "Sign in" submit.
function signInToken(page: Page) {
  // The password input lives under the "Admin token" label (htmlFor="token").
  return page.locator("#token");
}
function useTokenButton(page: Page) {
  // The token form's submit button reads exactly "Sign in" (the SSO button is a
  // distinct "Sign in with SSO"), so match exactly to avoid the SSO one.
  return page.getByRole("button", { name: "Sign in", exact: true });
}

// The console is reached once the Runs sidebar entry is visible. It's a
// react-router <NavLink> (role="link") and can carry a trailing attention-count
// badge (e.g. "Runs 2"), so match tolerantly rather than exact.
function runsNav(page: Page) {
  return page.getByRole("link", { name: /^Runs(\s+\d+)?$/ });
}

test.describe("auth / sign-in gate", () => {
  test("unauthenticated app shows the sign-in screen with the admin token field", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    // The Wardyn sign-in card renders the brand heading + token field + actions.
    await expect(page.getByRole("heading", { name: "Wardyn" })).toBeVisible();
    await expect(page.getByText("Admin token", { exact: true })).toBeVisible();
    await expect(signInToken(page)).toBeVisible();
    await expect(useTokenButton(page)).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign in with SSO" })).toBeVisible();

    // The console is NOT reachable while unauthenticated.
    await expect(runsNav(page)).toHaveCount(0);

    // No token should be stored yet.
    expect(await readToken(page)).toBeNull();
  });

  test("the token field is password-typed and the submit button is disabled while empty", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    const field = signInToken(page);
    await expect(field).toBeVisible();
    await expect(field).toHaveAttribute("type", "password");

    // Disabled with an empty value...
    await expect(useTokenButton(page)).toBeDisabled();
    // ...and enabled once something is typed.
    await field.fill("something");
    await expect(useTokenButton(page)).toBeEnabled();
  });

  test("a WRONG token is rejected with the error message and clears the stored token", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    await expect(signInToken(page)).toBeVisible();
    await signInToken(page).fill("definitely-not-the-admin-token");
    await useTokenButton(page).click();

    // probeAuth returns false (401) -> the SignIn screen surfaces an alert.
    const alert = page.getByRole("alert");
    await expect(alert).toBeVisible();
    await expect(alert).toContainText(/admin token was rejected/i);

    // We must remain on the gate (the console is never reached).
    await expect(runsNav(page)).toHaveCount(0);
    await expect(signInToken(page)).toBeVisible();

    // The rejected token must have been cleared so later requests don't carry it.
    expect(await readToken(page)).toBeNull();
  });

  test("typing again after a rejection dismisses the error", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    await signInToken(page).fill("bad-token");
    await useTokenButton(page).click();
    await expect(page.getByRole("alert")).toBeVisible();

    // Editing the field clears the error (onChange resets it).
    await signInToken(page).fill("bad-token-2");
    await expect(page.getByRole("alert")).toHaveCount(0);
  });

  test("the CORRECT token signs in and lands in the console (Runs sidebar visible)", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    await expect(signInToken(page)).toBeVisible();
    await signInToken(page).fill(GOOD_TOKEN);
    await useTokenButton(page).click();

    // probeAuth returns true (200) -> onSignIn() flips the app into the console.
    await expect(runsNav(page)).toBeVisible();
    // The sign-in gate is gone.
    await expect(signInToken(page)).toHaveCount(0);

    // The accepted token is persisted.
    expect(await readToken(page)).toBe(GOOD_TOKEN);
  });

  test("reload keeps the session (a stored token boots straight into the console)", async ({ page }) => {
    // Seed the good token, then boot like a returning operator.
    await bootWithStoredToken(page, GOOD_TOKEN);

    // Mount-time probeAuth succeeds, so we never see the gate.
    await expect(runsNav(page)).toBeVisible();
    await expect(signInToken(page)).toHaveCount(0);

    // Reloading must keep us signed in (token persisted in localStorage).
    await page.reload();
    await expect(runsNav(page)).toBeVisible();
    await expect(signInToken(page)).toHaveCount(0);
    expect(await readToken(page)).toBe(GOOD_TOKEN);
  });

  test("a stored but INVALID token boots to the sign-in gate, not the console", async ({ page }) => {
    // A revoked/garbage token in storage must fail the mount probe and gate us.
    await bootWithStoredToken(page, "stale-revoked-token");

    await expect(signInToken(page)).toBeVisible();
    await expect(runsNav(page)).toHaveCount(0);
  });

  test("sign-out returns to the sign-in gate and clears the token (/auth/logout HIGH fix)", async ({ page }) => {
    // Start signed in via a stored token.
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    // Open the user menu (top-right trigger holds the principal + chevron) and
    // sign out. The trigger has no role="button" semantics distinct from the
    // nav, so reach it via the principal label rendered inside it ("admin").
    const userMenuTrigger = page.locator("header button").filter({ hasText: "admin" });
    await expect(userMenuTrigger).toBeVisible();
    await userMenuTrigger.click();

    const signOut = page.getByRole("menuitem", { name: "Sign out" });
    await expect(signOut).toBeVisible();
    await signOut.click();

    // onSignOut: api.logout() (best-effort) -> setToken(null) -> gate.
    await expect(signInToken(page)).toBeVisible();
    await expect(runsNav(page)).toHaveCount(0);

    // The local admin token MUST be cleared so the next probe can't re-auth.
    expect(await readToken(page)).toBeNull();
  });

  test("after sign-out a reload stays on the gate (token really gone)", async ({ page }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    const userMenuTrigger = page.locator("header button").filter({ hasText: "admin" });
    await expect(userMenuTrigger).toBeVisible();
    await userMenuTrigger.click();
    const signOut = page.getByRole("menuitem", { name: "Sign out" });
    await expect(signOut).toBeVisible();
    await signOut.click();
    await expect(signInToken(page)).toBeVisible();

    // Reloading the cleared session must NOT silently re-sign us in.
    await page.reload();
    await expect(signInToken(page)).toBeVisible();
    await expect(runsNav(page)).toHaveCount(0);
    expect(await readToken(page)).toBeNull();
  });
});

// R4/F027 — a 5xx from the daemon is not a statement about the caller.
//
// probeAuth used to return a bare boolean, so a 500 on the mount probe was
// byte-identical to a 401: App.tsx set auth="unauthed" and rendered the gate,
// while the console's ONE reachability signal (refreshHealth / the unreachable
// banner) was itself gated behind auth==="authed" and so never ran. The console
// therefore told a signed-in operator they were signed out and offered no hint
// that anything was down. probeAuth now answers "unreachable" for that case and
// App.tsx records it on the same flag the banner reads, with the health poll
// running in every auth state so the flag clears itself.
//
// DEFERRED (Docker down for the R4 fix wave — never run, never skipped):
//   DOCKER_HOST=unix:///var/run/docker.sock WARDYN_E2E_ADDR=:8288 \
//   WARDYN_E2E_UI_ADDR=:8289 WARDYN_E2E_PG_CONTAINER=wardyn-profiles-pg \
//   WARDYN_E2E_PG_HOSTPORT=localhost:55434 ./scripts/run-ui-e2e.sh e2e/auth.spec.ts
test.describe("outage vs. rejection (R4/F027)", () => {
  test("a 5xx mount probe does not clear a stored token the daemon never rejected", async ({ page }) => {
    // The daemon is up enough to serve the console, but the runs list 500s —
    // a store outage, a rolling restart, a failing-over Postgres.
    await page.route("**/api/v1/runs?limit=1", (route) =>
      route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ error: "database unavailable" }),
      }),
    );
    await bootWithStoredToken(page, GOOD_TOKEN);

    // The gate is the honest screen (no verified session), but the token was
    // never rejected — a 401 clears it, a 500 must not.
    await expect(signInToken(page)).toBeVisible();
    expect(await readToken(page)).toBe(GOOD_TOKEN);
    // …and the console never claims the token was refused, because nothing
    // refused it.
    await expect(page.getByText(/admin token was rejected/i)).toHaveCount(0);
  });

  test("the gate keeps asking /healthz, so an SSO-only deployment is not left with no way in", async ({ page }) => {
    // /healthz is down for the gate's FIRST read only. The one-shot mount fetch
    // this replaced read that as sso:false and never asked again.
    let served = 0;
    await page.route("**/healthz", async (route) => {
      served += 1;
      if (served === 1) return route.fulfill({ status: 503, body: "" });
      return route.fallback();
    });
    await clearTokenInit(page);
    await page.goto("/");
    await expect(signInToken(page)).toBeVisible();

    // The SSO control is present either way; what must NOT happen is it being
    // frozen on the outage's answer. On an OIDC deployment the link appears on a
    // later poll; on a token-only one the button stays disabled — in both cases
    // the gate has asked more than once.
    await expect
      .poll(() => served, { timeout: 30_000 })
      .toBeGreaterThan(1);
  });
});

// R4/F116 — MID-SESSION EXPIRY, the one auth path no spec in either tier drove.
//
// wfetch routes every 401 to the module-level onUnauthorized handler, and
// App.tsx wires that to setAuth("unauthed") — the sign-in gate. It is the
// console's ONLY route back to a door the human can open once an SSO session
// dies or an admin token is revoked mid-work; without it the operator keeps a
// console that answers 401 to everything and never says why. Deleting the
// handler call left the whole vitest suite green, and every spec above boots
// EITHER already authenticated OR already rejected: none revokes a session that
// the console has already accepted.
//
// DEFERRED (Docker down for the R4 fix wave — never run, never skipped):
//   DOCKER_HOST=unix:///var/run/docker.sock WARDYN_E2E_ADDR=:8288 \
//   WARDYN_E2E_UI_ADDR=:8289 WARDYN_E2E_PG_CONTAINER=wardyn-profiles-pg \
//   WARDYN_E2E_PG_HOSTPORT=localhost:55434 ./scripts/run-ui-e2e.sh e2e/auth.spec.ts
test.describe("a session revoked mid-run (R4/F116)", () => {
  test("a 401 arriving on an ALREADY-authenticated console returns to the sign-in gate", async ({
    page,
  }) => {
    // In, the ordinary way: a stored token the daemon accepts.
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    // Now the session dies underneath the console: every API call 401s from
    // here on, exactly as it would after an SSO session expiry or a revoked
    // token. /healthz is left alone — the daemon is up, it is this CALLER who
    // is no longer welcome, and that is the distinction the gate must draw.
    await page.route("**/api/v1/**", (route) =>
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "unauthorized" }),
      }),
    );

    // The console's own polls (the attention badge / the run list) reach the
    // 401 on their own; a navigation guarantees a call without waiting one out.
    await runsNav(page).click();

    await expect(signInToken(page)).toBeVisible();
    // …and the gate is a real door, not a dead end: the submit control is there
    // to be used.
    await expect(useTokenButton(page)).toBeVisible();
  });
});
