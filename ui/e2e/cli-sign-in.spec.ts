/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// E2E for /cli/sign-in (ATT-LOGIN-UI, issue #3275; boards MATT2-*): the page `wardyn login` opens.
//
// The sign-in endpoints belong to the server lane (ATT-LOGIN-SRV) and a bearer-token harness cannot approve
// (mintGuards wants a browser session), so they are answered here in the server's wire shape
// (internal/types/cli_sign_in.go). What these specs prove is what the console does with each answer: Approve is a
// fetch POST after which the PAGE moves the browser to the CLI's loopback address; the address-mismatch warning
// shows and does not show; Deny; paste-back; the used, expired and refused pages.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { SIGN_IN as T } from "../src/app/components/wardyn/copy/cli-sign-in";

const ID = "5e1d9b0c-0000-4000-8000-000000000001";
const PORT = 53124;
const CALLBACK = `http://127.0.0.1:${PORT}/callback?id=${ID}&code=k-abc123`;
const DENIED = `http://127.0.0.1:${PORT}/callback?id=${ID}&error=denied`;

function view(over: Record<string, unknown> = {}) {
  return {
    id: ID,
    machine: "mbp-dev",
    user_code: "KQ7M-2XJD",
    mode: "loopback",
    store: "keychain",
    admin: false,
    request_address: "203.0.113.24",
    approver_address: "203.0.113.24",
    address_match: true,
    created_at: "2026-10-19T18:42:00Z",
    expires_at: "2026-10-19T18:52:00Z",
    session_ttl_seconds: 30 * 86400,
    session_idle_seconds: 7 * 86400,
    access_ttl_seconds: 3600,
    ...over,
  };
}

interface Stub {
  get?: { status?: number; json: unknown };
  approve?: { status?: number; json: unknown };
  deny?: { status?: number; json: unknown };
}

// Answers the three calls and records each one the page makes. The CLI's loopback listener is answered too, so
// a navigation to it is a navigation the spec can see end.
async function stubSignIn(page: Page, stub: Stub): Promise<string[]> {
  const calls: string[] = [];
  await page.route(`**/api/v1/me/cli-sign-ins/${ID}**`, async (route) => {
    const req = route.request();
    const leaf = new URL(req.url()).pathname.split("/").pop();
    const which = req.method() === "GET" ? "get" : leaf === "approve" ? "approve" : "deny";
    calls.push(`${req.method()} ${leaf}`);
    const answer = stub[which];
    if (!answer) return route.fulfill({ status: 500, json: { error: `unexpected ${which}` } });
    return route.fulfill({ status: answer.status ?? 200, json: answer.json });
  });
  await page.route(`http://127.0.0.1:${PORT}/**`, (route) =>
    route.fulfill({ status: 200, contentType: "text/html", body: "<!doctype html><title>CLI</title><p>CLI callback</p>" }),
  );
  return calls;
}

async function openSignIn(page: Page): Promise<void> {
  await gotoConsole(page, "user");
  await navToRoute(page, `/cli/sign-in?id=${ID}`);
}

const card = (page: Page) => page.getByRole("main").locator("section").first();

test.describe("/cli/sign-in", () => {
  test("shows the computer, address, time, code and the operator's own lifetimes, and nothing about a mismatch", async ({ page }) => {
    await stubSignIn(page, { get: { json: view() } });
    await openSignIn(page);

    await expect(page.getByRole("heading", { name: T.TITLE, level: 1 })).toBeVisible();
    const c = card(page);
    await expect(c.getByText(/^A terminal asked to sign in as .+\. Approve only if you just ran wardyn login yourself\.$/)).toBeVisible();
    await expect(c.getByText("mbp-dev", { exact: true })).toBeVisible();
    await expect(c.getByText("203.0.113.24", { exact: true })).toBeVisible();
    await expect(c.getByText("19 Oct 2026, 18:42 UTC", { exact: true })).toBeVisible();
    await expect(c.getByText("Expires in 10 minutes", { exact: true })).toBeVisible();
    await expect(c.getByTestId("cli-user-code")).toHaveText("KQ7M-2XJD");
    await expect(c.getByText(T.GRANT(3600, 30 * 86400, 7 * 86400), { exact: true })).toBeVisible();
    await expect(c.getByTestId("cli-address-mismatch")).toHaveCount(0);
    await expect(c.getByRole("button", { name: T.APPROVE })).toBeVisible();
    await expect(c.getByRole("button", { name: T.DENY })).toBeVisible();
  });

  test("Approve is a fetch POST, and after its 200 the page moves the browser to the loopback URL", async ({ page }) => {
    const calls = await stubSignIn(page, {
      get: { json: view() },
      approve: { json: { code: "k-abc123", code_expires_in: 60, redirect_url: CALLBACK } },
    });
    await openSignIn(page);
    const documentLoads: string[] = [];
    page.on("framenavigated", (f) => f === page.mainFrame() && documentLoads.push(f.url()));

    await card(page).getByRole("button", { name: T.APPROVE }).click();
    await page.waitForURL(CALLBACK);
    expect(calls.filter((c) => c.startsWith("POST"))).toEqual(["POST approve"]);
    // One navigation, to the CLI: the approve was not a form post and no redirect came from the server.
    expect(documentLoads).toEqual([CALLBACK]);
    await expect(page.getByText("CLI callback")).toBeVisible();
  });

  test("the mismatch warning names both addresses above Approve, and does not block it", async ({ page }) => {
    await stubSignIn(page, {
      get: { json: view({ request_address: "198.51.100.7", approver_address: "203.0.113.24", address_match: false }) },
      approve: { json: { code: "k-abc123", code_expires_in: 60, redirect_url: CALLBACK } },
    });
    await openSignIn(page);

    const c = card(page);
    const warning = c.getByTestId("cli-address-mismatch");
    await expect(warning).toContainText(T.MISMATCH_TITLE);
    await expect(warning).toContainText(T.MISMATCH_BODY);
    await expect(warning).toContainText(T.BROWSER_ADDRESS("203.0.113.24"));
    await expect(c.getByText("198.51.100.7", { exact: true })).toBeVisible();
    await expect(c.getByText(T.NOT_THIS_BROWSER)).toBeVisible();
    const above = await warning.evaluate((w) => {
      const approve = [...document.querySelectorAll("button")].find((b) => b.textContent === "Approve")!;
      return !!(w.compareDocumentPosition(approve) & Node.DOCUMENT_POSITION_FOLLOWING);
    });
    expect(above).toBe(true);

    await c.getByRole("button", { name: T.APPROVE }).click();
    await page.waitForURL(CALLBACK);
  });

  test("Deny tells the CLI through its callback", async ({ page }) => {
    const calls = await stubSignIn(page, { get: { json: view() }, deny: { json: { redirect_url: DENIED } } });
    await openSignIn(page);
    await card(page).getByRole("button", { name: T.DENY }).click();
    await page.waitForURL(DENIED);
    expect(calls.filter((c) => c.startsWith("POST"))).toEqual(["POST deny"]);
  });

  test("paste-back shows the code once, with the warning, and Deny shows the Denied page", async ({ page }) => {
    await stubSignIn(page, {
      get: { json: view({ mode: "paste" }) },
      approve: { json: { code: "7H4N-Q2VC", code_expires_in: 300 } },
    });
    await openSignIn(page);
    await card(page).getByRole("button", { name: T.APPROVE }).click();

    const c = card(page);
    await expect(page.getByRole("heading", { name: T.PASTE_TITLE, level: 1 })).toBeVisible();
    await expect(c.getByTestId("cli-paste-code")).toHaveText("7H4N-Q2VC");
    await expect(c.getByText(T.PASTE_WARN, { exact: true })).toBeVisible();
    await expect(c.getByText(T.PASTE_ONCE(300), { exact: true })).toBeVisible();
    await expect(c.getByRole("button", { name: T.PASTE_COPY })).toBeVisible();
    expect(new URL(page.url()).pathname).toBe("/cli/sign-in");
  });

  test("paste-back Deny shows the Denied page", async ({ page }) => {
    await stubSignIn(page, { get: { json: view({ mode: "paste" }) }, deny: { json: {} } });
    await openSignIn(page);
    await card(page).getByRole("button", { name: T.DENY }).click();
    await expect(page.getByRole("heading", { name: T.DENIED_TITLE, level: 1 })).toBeVisible();
    await expect(card(page).getByText(T.DENIED_BODY, { exact: true })).toBeVisible();
  });

  test("the admin step-up says Admin access for 1 hour", async ({ page }) => {
    await stubSignIn(page, { get: { json: view({ admin: true }) } });
    await openSignIn(page);
    await expect(page.getByRole("heading", { name: "Admin access for 1 hour", level: 1 })).toBeVisible();
    await expect(card(page).getByText(T.ADMIN_GRANT(3600), { exact: true })).toBeVisible();
  });

  for (const [what, reason, title, body] of [
    ["expired", "cli_sign_in_expired", T.EXPIRED_TITLE, T.EXPIRED_BODY],
    ["already used", "cli_sign_in_used", T.USED_TITLE, T.USED_BODY],
  ] as const) {
    test(`a request that is ${what} opens a page that says so and offers no Approve`, async ({ page }) => {
      await stubSignIn(page, { get: { status: 410, json: { error: "gone", reason } } });
      await openSignIn(page);
      await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
      await expect(card(page).getByText(body, { exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: T.APPROVE })).toHaveCount(0);
    });
  }

  for (const [what, status, reason, sentence] of [
    ["the user view (409)", 409, "api_token_member_mode_mint", T.REFUSED_USER_VIEW],
    ["no API-token permission (403)", 403, "capability_feature", T.REFUSED_NOT_ALLOWED],
  ] as const) {
    test(`Approve refused for ${what} says so in sign-in words and leaves the page as it was`, async ({ page }) => {
      const calls = await stubSignIn(page, {
        get: { json: view() },
        approve: { status, json: { error: "the server's own sentence", reason } },
      });
      await openSignIn(page);
      await card(page).getByRole("button", { name: T.APPROVE }).click();
      await expect(card(page).getByRole("alert").filter({ hasText: sentence })).toBeVisible();
      await expect(card(page).getByText("the server's own sentence")).toHaveCount(0);
      await expect(card(page).getByTestId("cli-user-code")).toHaveText("KQ7M-2XJD");
      await expect(card(page).getByRole("button", { name: T.APPROVE })).toBeEnabled();
      expect(calls.filter((c) => c.startsWith("POST"))).toEqual(["POST approve"]);
      expect(new URL(page.url()).pathname).toBe("/cli/sign-in");
    });
  }

  test("at phone width the page fits and Approve and Deny are 44 px tall", async ({ page }) => {
    await stubSignIn(page, { get: { json: view() } });
    await openSignIn(page);
    // After the console has loaded: the sidebar the helper waits for is not drawn at this width.
    await page.setViewportSize({ width: 390, height: 844 });
    const approve = card(page).getByRole("button", { name: T.APPROVE });
    await expect(approve).toBeVisible();
    for (const b of [approve, card(page).getByRole("button", { name: T.DENY })]) {
      const box = await b.boundingBox();
      expect(box!.height).toBeGreaterThanOrEqual(44);
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
  });
});
