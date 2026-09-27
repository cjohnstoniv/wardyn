/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, type Page } from "@playwright/test";
import { MEMBER_TOKEN, MEMBER_PRINCIPAL, consoleAPI } from "./fixtures";
import { SHELL } from "../src/app/components/wardyn/copy";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { SIGNIN } from "../src/app/lib/sign-in-copy";
import { SIGNIN_HELP_LINK_LABEL } from "../src/app/lib/people-access-copy";
import { SESSION_ENDED_REASON } from "../src/app/lib/api/core";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_DRAFT } from "../src/app/lib/reauth-copy";

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
    // #457: this harness has no OIDC configured — no disabled SSO stub, no
    // control at all.
    await expect(page.getByRole("button", { name: "Sign in with SSO" })).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Sign in with SSO" })).toHaveCount(0);

    // The console is NOT reachable while unauthenticated.
    await expect(runsNav(page)).toHaveCount(0);

    // No token should be stored yet.
    expect(await readToken(page)).toBeNull();

    // H1: the cold mount probe (no session ever established this tab) is
    // ALSO a 401 — onUnauthorized used to fire unconditionally, so this
    // ordinary first-visit gate told a human who never had a session that it
    // ended. No alert and no notice at all on this path.
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByText(SESSION_ENDED_REASON)).toHaveCount(0);
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

  test("a per-person token works through the token field", async ({ page }) => {
    await page.goto("/");
    await signInToken(page).fill(MEMBER_TOKEN);
    await useTokenButton(page).click();
    await expect(runsNav(page)).toBeVisible();
    await expect(signInToken(page)).toHaveCount(0);

    for (const reload of [false, true]) {
      if (reload) await page.reload();
      await expect(runsNav(page)).toBeVisible();
      const response = await consoleAPI(page, "GET", "/api/v1/me");
      expect(response.status).toBe(200);
      expect(JSON.parse(response.text)).toMatchObject({
        principal: MEMBER_PRINCIPAL, method: "token", role: "user",
        operator: false, security_operator: false, user_type: { id: "standard" },
      });
      expect((await consoleAPI(page, "GET", "/api/v1/site-config")).status).toBe(403);
    }
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
    // #483: a session this browser held, refused on load — said as an amber
    // warning (role=status), never in the error box.
    await expect(page.getByRole("status").filter({ hasText: SESSION_ENDED_REASON })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
  });

  test("sign-out returns to the sign-in gate and clears the token (/auth/logout HIGH fix)", async ({ page }) => {
    // Start signed in via a stored token.
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    // Open the user menu (top-right trigger holds the principal + chevron) and
    // sign out. The trigger has no role="button" semantics distinct from the
    // nav, so reach it as the header's one menu trigger (the view switch sits beside it).
    const userMenuTrigger = page.locator('header button[aria-haspopup="menu"]');
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
    // #483: a deliberate sign-out is not a session that ended — no notice.
    await expect(page.getByText(SESSION_ENDED_REASON)).toHaveCount(0);
  });

  test("after sign-out a reload stays on the gate (token really gone)", async ({ page }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    const userMenuTrigger = page.locator('header button[aria-haspopup="menu"]');
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

// #378/#379 — the SSO-only screen shape, driven by a mocked /healthz.
//
// The server-side refusal (validateSSOOnlyPosture, cmd/wardynd/boot_posture.go)
// is proven by the Kubernetes SSO walk, not hermetically here (#379's own
// acceptance says so) — this spec instead pins what the CONSOLE does once the
// daemon reports the posture: /healthz's token_login/sso_only bits, mocked at
// the network boundary the same way the outage spec below mocks **/healthz.
test.describe("SSO-only screen shape (#378/#379)", () => {
  test("sso_only:true, token_login:false renders one 'Sign in with SSO' button and nothing else", async ({ page }) => {
    await page.route("**/healthz", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "ok", sso: true, sso_only: true, token_login: false }),
      }),
    );
    await clearTokenInit(page);
    await page.goto("/");

    const ssoLink = page.getByRole("link", { name: "Sign in with SSO" });
    await expect(ssoLink).toBeVisible();
    await expect(ssoLink).toHaveAttribute("href", "/auth/login");

    // No admin-token form: neither the field nor its label/instructions.
    await expect(signInToken(page)).toHaveCount(0);
    await expect(page.getByText("Admin token", { exact: true })).toHaveCount(0);

    // No role-source caveat — #457 removed it everywhere.
    await expect(page.getByText(/comes from your SSO role assignment/i)).toHaveCount(0);
  });

  // #457: the role-source caveat is gone from every cell now, not just sso_only.
  test("sso:true, sso_only:false, token_login:false: the admin-token form is gone, no role-source caveat", async ({ page }) => {
    await page.route("**/healthz", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "ok", sso: true, sso_only: false, token_login: false }),
      }),
    );
    await clearTokenInit(page);
    await page.goto("/");

    await expect(page.getByRole("link", { name: "Sign in with SSO" })).toBeVisible();
    await expect(signInToken(page)).toHaveCount(0);
    await expect(page.getByText(/comes from your SSO role assignment/i)).toHaveCount(0);
  });
});

// #212/#457 — a sign-in refusal must not advertise a working credential or
// hand an unauthenticated reader an env var they cannot reach. These pin the
// words the person actually reads, importing SIGNIN rather than duplicating
// the literal.
test.describe("sign-in refusals name Wardyn and point this reader at what they can do (#212)", () => {
  test("the admin-token field starts empty, with no working demo credential in the placeholder", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/");

    const field = signInToken(page);
    await expect(field).toBeVisible();
    await expect(field).not.toHaveAttribute("placeholder");
  });

  test("an unreachable daemon names Wardyn, not a bare 'control plane' dead end", async ({ page }) => {
    await clearTokenInit(page);
    // Simulate a network failure on the token-probe request the same way a
    // daemon that never answers would: the fetch itself never resolves ok.
    await page.route("**/api/v1/runs?limit=1", (route) => route.abort());
    await page.goto("/");

    await signInToken(page).fill("sometoken");
    await useTokenButton(page).click();

    const alert = page.getByRole("alert");
    await expect(alert).toBeVisible();
    await expect(alert).toHaveText(SIGNIN.UNREACHABLE_ERROR);
  });

  test("the email_domain refusal points a locked-out reader at their admin, not an env var they cannot reach", async ({ page }) => {
    await clearTokenInit(page);
    await page.goto("/?auth_error=email_domain");

    const alert = page.getByRole("alert");
    await expect(alert).toBeVisible();
    await expect(alert).toHaveText(SIGNIN.EMAIL_DOMAIN);
    await expect(alert).not.toContainText("WARDYN_OIDC_EMAIL_DOMAINS");
  });
});

// #484 — the admin-written request-access help, published on the anonymous
// /healthz and shown under the four refusals a person cannot clear alone.
// /healthz is mocked at the network boundary, like the SSO-only specs above.
test.describe("admin-written help under a sign-in refusal (#484)", () => {
  const HELP_TEXT = `Ask in #it-helpdesk — it's "Wardyn access" you want. <b>not bold</b>`;
  const HELP_URL = "https://it.corp.example/request";

  async function mockHelp(page: Page): Promise<void> {
    await page.route("**/healthz", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          status: "ok",
          sso: true,
          token_login: true,
          sign_in_help_text: HELP_TEXT,
          sign_in_help_url: HELP_URL,
        }),
      }),
    );
  }

  test("the no_role refusal shows Wardyn's sentence first, then the admin's text and Request access", async ({ page }) => {
    await mockHelp(page);
    await clearTokenInit(page);
    await page.goto("/?auth_error=no_role");

    const alert = page.getByRole("alert");
    await expect(alert.locator("..")).toMatchAriaSnapshot(`
      - alert: ${JSON.stringify(SIGNIN.NO_ROLE)}
      - paragraph: ${JSON.stringify(HELP_TEXT)}
      - link "${SIGNIN_HELP_LINK_LABEL}":
        - /url: ${HELP_URL}
    `);
    await expect(alert).toHaveText(SIGNIN.NO_ROLE);
    const help = page.getByTestId("sign-in-help");
    // Literal text: the markup in it is characters, never an element.
    await expect(help.getByText(HELP_TEXT, { exact: true })).toBeVisible();
    await expect(help.locator("b")).toHaveCount(0);
    const link = help.getByRole("link", { name: SIGNIN_HELP_LINK_LABEL });
    await expect(link).toHaveAttribute("href", HELP_URL);
    await expect(link).toHaveAttribute("target", "_blank");
    await expect(link).toHaveAttribute("rel", "noopener noreferrer");
    // Wardyn's own sentence stays first.
    const [alertBox, helpBox] = await Promise.all([alert.boundingBox(), help.boundingBox()]);
    expect(alertBox && helpBox && helpBox.y > alertBox.y).toBe(true);
  });

  test("a timeout refusal gets nothing extra", async ({ page }) => {
    await mockHelp(page);
    await clearTokenInit(page);
    await page.goto("/?auth_error=oidc_transient");

    await expect(page.getByRole("alert")).toBeVisible();
    await expect(page.getByRole("link", { name: "Sign in with SSO" })).toBeVisible();
    await expect(page.getByTestId("sign-in-help")).toHaveCount(0);
    await expect(page.getByText(HELP_TEXT)).toHaveCount(0);
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
test.describe("outage vs. rejection", () => {
  // ticket: R4/F027
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
    // /healthz is down until `healthy` flips. The one-shot mount fetch this
    // replaced read the outage as sso:false and never asked again.
    //
    // No request count: App.tsx polls /healthz itself (HEALTH_POLL_MS) in
    // every auth state, so "served more than once" holds with the gate's own
    // re-ask paused. What is pinned is the gate's own reads: the outage on
    // screen first, then the recovery reaching the screen ONLY through the
    // gate's poll. page.clock drives SSO_POLL_MS (sign-in.tsx) instead of a
    // real 10s setInterval racing a loaded CI host.
    await page.clock.install();
    let healthy = false;
    await page.route("**/healthz", (route) =>
      healthy
        ? route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok" }) })
        : route.fulfill({ status: 503, body: "" }),
    );
    await clearTokenInit(page);
    await page.goto("/");
    // #457: no answer yet, so the gate is honestly "checking" and offers no door.
    await expect(page.getByRole("status").filter({ hasText: SIGNIN.CHECKING })).toBeVisible();
    await expect(signInToken(page)).toBeHidden();

    // Only the gate's own unanswered reads (mount + two SSO_POLL_MS ticks)
    // turn "checking" into "still checking" — so its mount read has landed on
    // the outage before `healthy` flips below. runFor, not fastForward: the
    // latter fires a due interval at most once.
    await page.clock.runFor("00:21");
    await expect(page.getByRole("status").filter({ hasText: SIGNIN.STILL_CHECKING })).toBeVisible();
    await expect(signInToken(page)).toBeHidden();

    // The daemon comes back. The posture must not stay frozen on the outage:
    // one more SSO_POLL_MS tick and the gate offers the token door.
    healthy = true;
    await page.clock.fastForward("00:11");
    await expect(signInToken(page)).toBeVisible();
  });
});

// R4/F116 — MID-SESSION EXPIRY, the one auth path no spec in either tier drove.
//
// wfetch routes every 401 to the module-level onUnauthorized handler. Since
// #483 a 401 on a console that WAS signed in keeps the page and opens the
// "Sign in to continue" dialog over it — the console's only way back to a
// door the human can open once an SSO session dies or an admin token is
// revoked mid-work; without it the operator keeps a console that answers 401
// to everything and never says why. Every other spec here boots EITHER already
// authenticated OR already rejected: none revokes a session the console has
// already accepted. The draft-keeping half is reauth-in-place.spec.ts.
function revokeEverything(page: Page) {
  return page.route("**/api/v1/**", (route) =>
    route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({ error: "unauthorized" }) }),
  );
}
function reauthDialog(page: Page) {
  return page.getByRole("dialog", { name: REAUTH_DIALOG.TITLE });
}
async function signInInDialog(page: Page) {
  await reauthDialog(page).locator("#reauth-token").fill(GOOD_TOKEN);
  await reauthDialog(page).getByRole("button", { name: REAUTH_BAR.CTA, exact: true }).click();
}

test.describe("a session revoked mid-run", () => {
  // ticket: R4/F116, #483
  test("a 401 arriving on an ALREADY-authenticated console opens the sign-in dialog over the page", async ({
    page,
  }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    // The session dies underneath the console. /healthz is left alone — the
    // daemon is up, it is this CALLER who is no longer welcome. The board's
    // own poll reaches the 401 on its own, so the door is asserted directly,
    // with room for one full poll period.
    await revokeEverything(page);
    await expect(reauthDialog(page)).toBeVisible({ timeout: 15_000 });
    // The page stayed: no full sign-in screen replaced it…
    await expect(signInToken(page)).toHaveCount(0);
    // …and the dialog is a real door, not a dead end.
    await expect(reauthDialog(page).getByRole("button", { name: REAUTH_BAR.CTA, exact: true })).toBeVisible();
  });
});

// M2: the page belongs to whoever was signed in BEFORE — the same person
// with a narrower role is told so and taken to Runs (owner decision Q457-9).
test.describe("the page is checked against the re-authenticated role", () => {
  // ticket: M2, #483
  test("a member signing back in over an admin-only page is told so and taken to Runs", async ({ page }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();
    await page.goto("/admin/drives");
    await expect(page.getByRole("heading", { name: "User drives", level: 1 })).toBeVisible();

    await revokeEverything(page);
    await expect(reauthDialog(page)).toBeVisible({ timeout: 15_000 });

    // Back as a MEMBER (the harness's bearer token is always admin
    // server-side — splice GET /me the same way mockMemberRole does).
    await page.unroute("**/api/v1/**");
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.role = "user";
      json.operator = false;
      json.security_operator = false;
      await route.fulfill({ response, json });
    });
    await signInInDialog(page);

    await expect(reauthDialog(page).getByText(REAUTH_DIALOG.ROLE_CHANGED_BODY)).toBeVisible();
    await reauthDialog(page).getByRole("button", { name: REAUTH_DRAFT.GO_TO_RUNS }).click();
    await expect(page).toHaveURL(/\/runs$/);
    await expect(reauthDialog(page)).toHaveCount(0);
  });

  test("neg: the SAME role signing back in stays on the page", async ({ page }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();
    await page.goto("/admin/drives");
    await expect(page.getByRole("heading", { name: "User drives", level: 1 })).toBeVisible();

    await revokeEverything(page);
    await expect(reauthDialog(page)).toBeVisible({ timeout: 15_000 });
    await page.unroute("**/api/v1/**");
    await signInInDialog(page);

    await expect(reauthDialog(page)).toHaveCount(0);
    await expect(page).toHaveURL(/\/admin\/drives$/);
    await expect(page.getByRole("heading", { name: "User drives", level: 1 })).toBeVisible();
  });

  // T-25 (P2-5's sibling gap): roleCanReach's OPERATOR_ONLY_PREFIXES
  // (App.tsx) once named /drives, which would have bounced a security admin
  // here to Runs exactly like the member case above — the wrong answer, since
  // a security admin manages Drives' grants and preview (securityOps,
  // admin-member-modes-design.md §2 comment). Only /admin/providers is
  // operator-only now; this pins the OTHER admin tier through the same
  // captured-path door the "neg" case above proves for a plain admin.
  test("a security admin re-authenticating over their own captured /admin/drives path is restored, not bounced to Runs", async ({ page }) => {
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.role = "security_admin";
      json.operator = false;
      json.security_operator = true;
      await route.fulfill({ response, json });
    });
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    await page.goto("/admin/drives");
    await expect(page.getByRole("heading", { name: "User drives", level: 1 })).toBeVisible();

    // Same trigger as the "neg" case above (revokeEverything): the console is
    // already authed, so this stays the in-place reauth DIALOG, not the full
    // sign-in gate — the wrong locators here (signInToken/useTokenButton, the
    // gate's own) made this test wait on a screen that never replaces the
    // page, timing out even though the dialog was up the whole time.
    await revokeEverything(page);
    await expect(reauthDialog(page)).toBeVisible({ timeout: 15_000 });

    // Drops only the 401 handler: the /me splice above still answers the re-auth.
    await page.unroute("**/api/v1/**");
    await signInInDialog(page);

    await expect(reauthDialog(page)).toHaveCount(0);
    await expect(page).toHaveURL(/\/admin\/drives$/);
    await expect(page.getByRole("heading", { name: "User drives", level: 1 })).toBeVisible();
  });
});

// B1 — the console fails CLOSED, never open, when /me never answers.
//
// A stored admin token still authenticates (probeAuth hits GET /runs, not
// /me), so the shell mounts — but app-shell.tsx's own effect calls GET /me
// separately for role/identity, and whoami() (lib/api/health.ts) folds ANY
// non-ok response to null. Before the fix, `role` defaulted "admin" and
// `roleResolved` meant only "the fetch settled" — so a failed /me rendered
// the FULL admin nav off a guess, indistinguishable from an authz breach.
// identityResolved now gates the nav directly: settled-but-unknown renders
// NEITHER nav set, and the banner below is the whole page.
test.describe("settled-but-unknown identity (a failed /me renders no nav, not a guess)", () => {
  // ticket: B1
  test("a 500 on /me shows the identity-unknown banner, no admin nav and no member nav", async ({ page }) => {
    let meFailing = true;
    await page.route("**/api/v1/me", (route) => {
      if (!meFailing) return route.fallback();
      return route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ error: "boom" }),
      });
    });
    await bootWithStoredToken(page, GOOD_TOKEN);

    // The banner IS the page — role="status", never a spinner, never a
    // silent guess.
    const banner = page.getByRole("status").filter({ hasText: SHELL.UNKNOWN_BODY });
    await expect(banner).toBeVisible();

    // Neither the admin nav (Audit, Policies, Secrets, Permissions,
    // Governance) nor the member nav (Runs, Approvals, Workspaces) renders —
    // settled-but-unknown is its own third state, not a fallback to either.
    for (const label of ["Runs", "Approvals", "Workspaces", "Audit", "Policies", "Secrets", "Permissions", "Governance", "Recordings"]) {
      await expect(page.getByRole("link", { name: new RegExp(`^${label}`) })).toHaveCount(0);
    }

    // Retry re-fires whoami() and, once /me answers, resolves the real nav —
    // this was never a dead end.
    meFailing = false;
    await banner.getByRole("button", { name: SHELL.UNKNOWN_ACTION }).click();
    await expect(runsNav(page)).toBeVisible();
    await expect(banner).toHaveCount(0);
  });
  // VL-26 (V1 lens D): the gate is the ROUTE SHELL, not the nav. A person who
  // types /admin/settings (or any admin route) while /me is refused gets the
  // same banner and no screen — before the fix the nav was hidden but the
  // route still painted operator controls off the fail-open context default.
  test("a 500 on /me shows the banner on /admin/settings and /admin/governance — no screen paints for an unknown identity", async ({ page }) => {
    let meFailing = true;
    await page.route("**/api/v1/me", (route) => {
      if (!meFailing) return route.fallback();
      return route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) });
    });
    await bootWithStoredToken(page, GOOD_TOKEN);

    for (const path of ["/admin/settings", "/admin/governance"]) {
      await page.goto(path);
      await expect(page.getByRole("status").filter({ hasText: SHELL.UNKNOWN_BODY })).toBeVisible();
      await expect(page.getByRole("heading", { name: "Settings" })).toHaveCount(0);
      await expect(page.getByRole("heading", { name: GOV.TITLE })).toHaveCount(0);
    }

    // Once /me answers, the SAME route paints its real screen — the gate was
    // about the identity, never about the path.
    meFailing = false;
    await page.getByRole("status").filter({ hasText: SHELL.UNKNOWN_BODY }).getByRole("button", { name: SHELL.UNKNOWN_ACTION }).click();
    await expect(page.getByRole("heading", { name: GOV.TITLE })).toBeVisible();
  });

});

// R4-F107 — a failed sign-out says so, rather than a console.error nobody
// sees. POST /auth/logout is best-effort (health.logout() returns a bool, never
// throws), and the local admin token is always dropped either way — the
// question this answers is whether the SERVER-side OIDC session might still
// be live, which matters on a shared machine.
test.describe("a failed sign-out is surfaced, not swallowed", () => {
  // ticket: R4-F107
  test("POST /auth/logout failing still drops the local session, but toasts that the server wasn't confirmed", async ({
    page,
  }) => {
    await bootWithStoredToken(page, GOOD_TOKEN);
    await expect(runsNav(page)).toBeVisible();

    await page.route("**/api/v1/auth/logout", (route) =>
      route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) }),
    );

    const userMenuTrigger = page.locator('header button[aria-haspopup="menu"]');
    await userMenuTrigger.click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();

    // The gate returns regardless — this tab really is signed out.
    await expect(signInToken(page)).toBeVisible();
    expect(await readToken(page)).toBeNull();

    // …but the toast says what a silent success would have hidden: the
    // server-side session was never confirmed dead.
    await expect(page.getByText(SHELL.SIGN_OUT_FAILED_TITLE)).toBeVisible();
    await expect(page.getByText(SHELL.SIGN_OUT_FAILED_BODY)).toBeVisible();
  });
});
