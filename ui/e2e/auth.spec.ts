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
  return page.evaluate((key) => localStorage.getItem(key), TOKEN_KEY);
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
