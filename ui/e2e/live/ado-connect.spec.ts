/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE ADO KIND WALK'S BROWSER LEG (#751).
 *
 * scripts/lib/kind-sso-walk-ado.sh is curl-only: every sign-in is three GETs
 * against the fake's picker, so nothing ever drove the console's own Azure
 * DevOps connect UI. That connect flow (use-ado-connect.ts, #386) opens its
 * popup on about:blank and must move it on at once, never leaving the
 * person sitting on a blank placeholder tab; and a BLOCKED popup falls back
 * to a plain link rather than strand the dialog (#386 review finding F9),
 * worded since #628 as "Your browser blocked the connect popup." plus
 * "Open Azure DevOps sign-in". Neither had a browser anywhere near a real
 * backend to prove it against. This file is that browser: a real Chromium,
 * signed in through the SAME fake Entra tenant the curl walk uses, driving
 * the SAME /settings connect button the curl walk never touches.
 *
 * What only a live cluster proves that ui/e2e/ado-getting-started.spec.ts's
 * mocked popup case cannot: that the daemon's real redirect
 * (/api/v1/scm/azure-devops/signin) actually reaches a real Microsoft Entra
 * endpoint — the popup's own URL ends up on login.microsoftonline.com, not on
 * an unreachable address a mock never has to resolve.
 *
 * `/setup/status`'s `scm_access` is still route-mocked (the technique
 * ui/e2e/ado-getting-started.spec.ts and ui/e2e/ado-launch-door.spec.ts
 * already use: fetch the real response, splice one field, fulfill) — that is
 * what makes the "Connect Azure DevOps" button appear on demand, on a real
 * signed-in session, without first having to manufacture the CAUSE_ROW_IS_NEWER
 * precondition (a Wardyn session that predates the admin's Azure DevOps row)
 * against the walk's fixed two-identity fixture. Splicing one field the wire
 * already carries is not inventing a UI state; the button and its fallback
 * are the real components, rendering real copy, against a real backend.
 *
 * The sandbox-progress half of #751's "wanted" list ("where the profile
 * launches a sign-in sandbox, it asserts the door's progress steps") does not
 * apply here: unlike the AWS SSO door (ui/e2e/live/helpers.ts's
 * awaitSelfRunStarted), Azure DevOps connect never launches a sign-in
 * sandbox — it is a popup through a plain Entra OAuth redirect. There is no
 * sandbox door on this profile to assert progress on.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as every other file here.
 */

import { expect, test, type Page } from "@playwright/test";
import { ADO } from "../../src/app/lib/ado-entra-copy";

test.skip(
  process.env.WARDYN_TEST_K8S !== "1",
  "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/lib/kind-sso-walk-ado.sh)",
);

// scripts/lib/kind-sso-walk-ado.sh's own fixed member identity/proxy —
// exported by the walk as WARDYN_LIVE_ADO_*. The literal fallback matches
// the walk script exactly, so a developer running this file by hand against
// a walk-up cluster (without re-exporting anything the walk already set)
// still works.
const MEMBER_EMAIL = process.env.WARDYN_LIVE_ADO_MEMBER_EMAIL || "member@wardyn.test";
// The walk's port-forward to the fake (kind-sso-walk-ado.sh's FAKE_LOCAL) —
// the browser's only route to "login.microsoftonline.com", exactly as the
// curl walk's --proxy is. Required; there is no meaningful fallback for a
// proxy address.
const PROXY_URL = process.env.WARDYN_LIVE_ADO_PROXY_URL || "";

// Routed through the fake's forward proxy, bypassed for the console itself
// (localhost) — the same split kind-sso-walk-ado.sh's curl draws between
// "the console hop is direct" and "the Microsoft hop goes through the fake's
// proxy". ignoreHTTPSErrors: the fake's leaves are signed by the walk's own
// CA, which this browser has no reason to have imported into its trust store.
test.use({
  proxy: PROXY_URL ? { server: PROXY_URL, bypass: "localhost,127.0.0.1" } : undefined,
  ignoreHTTPSErrors: true,
});

/**
 * Sign in through the fake Entra tenant's picker, in the browser — the same
 * three hops kind-sso-walk-ado.sh's `signin()` drives with curl
 * (/auth/login -> the picker -> the console callback), driven instead by
 * real navigation and a real click. The console's own login page is
 * provider-agnostic (dexSignIn in ./helpers.ts uses the identical "Sign in
 * with SSO" entry point against Dex); only the picker in between differs,
 * and it is plain HTML — one link per identity, text is the login (see
 * test/entrafake/server.go's pickIdentity).
 */
async function entraSignIn(page: Page, email: string): Promise<void> {
  await page.goto("/");
  await page
    .getByRole("link", { name: "Sign in with SSO" })
    .or(page.getByRole("button", { name: "Sign in with SSO" }))
    .first()
    .click();
  await page.getByRole("link", { name: email, exact: true }).click();
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible({ timeout: 60_000 });
}

/**
 * Force the ADO connect CTA to appear (the technique
 * ui/e2e/ado-getting-started.spec.ts's own mockScmAccess uses): fetch the
 * REAL /setup/status this cluster answers, splice `scm_access`, fulfill.
 * Everything else in the response — model access, the workspace roster,
 * everything the member's real per-user rows say — stays exactly what the
 * daemon served.
 *
 * GET /me/scm-access is answered "no row" to match: the member's own
 * browser sign-in in entraSignIn already captured their credential (the
 * curl walk's step 5), so the real endpoint reads "live", and
 * use-ado-connect.ts's poll closes the popup on its first "live" tick —
 * which, left real, would race the popup's trip to the fake's picker.
 */
async function mockScmAccess(page: Page, scm_access: Record<string, unknown>): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.scm_access = scm_access;
    await route.fulfill({ response, json });
  });
  await page.route("**/api/v1/me/scm-access", (route) => route.fulfill({ json: [] }));
}

const NOT_CONNECTED = { state: "not_configured", cause: "row_is_newer" };

test.describe.configure({ mode: "serial" });

test.beforeEach(() => {
  expect(
    PROXY_URL,
    "WARDYN_LIVE_ADO_PROXY_URL is unset — run this through scripts/lib/kind-sso-walk-ado.sh",
  ).not.toBe("");
});

test.describe("the ADO kind walk's browser leg (#751)", () => {
  test("connect never leaves an about:blank tab for the person to sit on", async ({ page, context }) => {
    await entraSignIn(page, MEMBER_EMAIL);
    await mockScmAccess(page, NOT_CONNECTED);
    await page.goto("/settings#azure-devops");
    const cta = page.getByRole("button", { name: ADO.CONNECT_ADO });
    await expect(cta).toBeVisible();

    // use-ado-connect.ts's connect() opens about:blank FIRST, same tick, to
    // sever `opener` before navigating (the tabnabbing fix) — so the real
    // assertion is not "it never opens about:blank" but "it never SITS
    // there": the popup's first navigation, within a second of the click,
    // is the console's own sign-in door. about:blank itself makes no
    // request, so the first navigation request any page but this one makes
    // is where the popup went next.
    const popupNavs: string[] = [];
    context.on("request", (req) => {
      if (req.isNavigationRequest() && req.frame().page() !== page) popupNavs.push(req.url());
    });
    const [popup] = await Promise.all([context.waitForEvent("page"), cta.click()]);
    await expect
      .poll(() => popupNavs[0] ?? "", { timeout: 1_000, message: "popup sat on about:blank" })
      .toMatch(/\/api\/v1\/scm\/azure-devops\/signin$/);
    // And it reached the real fake, not an address a mock never has to
    // resolve — the one thing only a live cluster proves.
    await popup.waitForURL(/login\.microsoftonline\.com/, { timeout: 30_000 });
    await popup.close();
  });

  test("a blocked popup falls back to the #628 blocked line and \"Open Azure DevOps sign-in\" link", async ({ page, context }) => {
    await entraSignIn(page, MEMBER_EMAIL);
    // Nothing outside a real browser process can make the OS popup blocker
    // fire, so this simulates the one thing it would do — the same
    // simulation use-ado-connect.test.ts's own unit test uses (review
    // finding F1: window.open returning null is neither a connection nor a
    // decline).
    await page.addInitScript(() => {
      window.open = () => null;
    });
    await mockScmAccess(page, NOT_CONNECTED);
    await page.goto("/settings#azure-devops");
    const cta = page.getByRole("button", { name: ADO.CONNECT_ADO });
    await expect(cta).toBeVisible();

    await cta.click();

    // The fallback (#386 F9, #628's copy): the popup-blocked line, plus a
    // plain link the person can open themselves — never a dialog stranded
    // with no way forward.
    await expect(page.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeVisible();
    const fallback = page.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    await expect(fallback).toBeVisible();
    await expect(fallback).toHaveAttribute("href", /\/api\/v1\/scm\/azure-devops\/signin/);

    // The fallback link itself opens sign-in in a real tab the browser owns
    // (connectFallback's own poll, review follow-up N1) — following it
    // reaches the same real fake the first case did.
    const [tab] = await Promise.all([context.waitForEvent("page"), fallback.click()]);
    await tab.waitForURL(/login\.microsoftonline\.com/, { timeout: 30_000 });
    await tab.close();
  });
});
