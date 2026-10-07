/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, Request } from "@playwright/test";
import { ADMIN_TOKEN, expect, gotoConsole, navToRoute, test } from "./fixtures";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_RENEW } from "../src/app/lib/reauth-copy";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";

// #483 — a session that ends mid-page keeps the page. A real screen with a
// typed-but-unsaved draft (/admin/providers, a git row's base URL), a Save whose
// PUT hits the expiry (route interception answers it 401), and the dialog
// that opens over it.

const BASE_URL = "https://github.com/acme-reauth";

const isProvidersPut = (r: Request) => r.method() === "PUT" && new URL(r.url()).pathname === "/api/v1/workspace-providers";

/** On /admin/providers with a git row typed but not saved; the Save then 401s. */
async function saveIntoAnExpiredSession(page: Page): Promise<{ puts: () => number }> {
  let puts = 0;
  page.on("request", (r) => {
    if (isProvidersPut(r)) puts += 1;
  });
  await gotoConsole(page);
  await navToRoute(page, "/admin/providers");
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).click();
  await page.getByTestId("provider-row-github").locator("textarea").fill(BASE_URL);

  await page.route("**/api/v1/workspace-providers", (route) =>
    route.request().method() === "PUT"
      ? route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({ error: "unauthorized" }) })
      : route.fallback(),
  );
  await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
  // Room for a cold lazy chunk on a loaded box — the dialog is its own chunk.
  await expect(page.getByRole("dialog", { name: REAUTH_DIALOG.TITLE })).toBeVisible({ timeout: 15_000 });
  await page.unroute("**/api/v1/workspace-providers");
  return { puts: () => puts };
}

async function signInInDialog(page: Page): Promise<void> {
  const dialog = page.getByRole("dialog", { name: REAUTH_DIALOG.TITLE });
  await dialog.locator("#reauth-token").fill(ADMIN_TOKEN);
  await dialog.getByRole("button", { name: REAUTH_BAR.CTA, exact: true }).click();
}

test.describe("signed out mid-page: sign in again in place (#483)", () => {
  test("the dialog opens over the page, and after signing in the typed draft is still there", async ({ page }) => {
    const { puts } = await saveIntoAnExpiredSession(page);
    await expect(page.getByText(REAUTH_DIALOG.BODY)).toBeVisible();
    // The page never left: no full sign-in screen behind the dialog.
    await expect(page.locator("#token")).toHaveCount(0);

    await signInInDialog(page);
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByTestId("provider-row-github").locator("textarea")).toHaveValue(BASE_URL);
    await expect(page).toHaveURL(/\/providers$/);
    // The refused save is never re-sent — the screen says so beside Save.
    await expect(page.getByText(REAUTH_DIALOG.WRITE_DROPPED)).toBeVisible();
    expect(puts()).toBe(1);
  });

  test("someone else signing in reloads the page fresh as them — the draft is gone", async ({ page }) => {
    await saveIntoAnExpiredSession(page);
    // The console tells principals apart by GET /me — splice a different one
    // onto the real answer (the harness has one admin token).
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.principal = "someone-else";
      await route.fulfill({ response, json });
    });
    await page.evaluate(() => {
      (window as unknown as { beforeReauth?: boolean }).beforeReauth = true;
    });

    await signInInDialog(page);
    // A fresh document: the marker set on the old one is gone.
    await expect
      .poll(() => page.evaluate(() => (window as unknown as { beforeReauth?: boolean }).beforeReauth ?? false))
      .toBe(false);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
    await expect(page.getByText(REAUTH_DIALOG.WRITE_DROPPED)).toHaveCount(0);
  });
});

// The expiry banner's "Sign in again", renewing in place. The harness signs in
// with one admin token and has no SSO, so GET /me is spliced: it gains the
// session_expires_at that draws the banner, and later whatever a sign-in in
// the popup would have changed. Nothing the popup shows is read.

// app-shell.tsx SESSION_EXPIRY_COPY.soon[0] (the shell is not importable here).
const EXPIRING_SOON = "Your session is expiring soon.";
const inMinutes = (m: number) => new Date(Date.now() + m * 60_000).toISOString();
const isMe = (url: string) => new URL(url).pathname === "/api/v1/me";

/** On /admin/providers with a git row typed but not saved, under a session
 *  two minutes from its end. `answer` changes what /me says from then on. */
async function draftUnderAnExpiringSession(page: Page): Promise<{ answer: (over: Record<string, unknown>) => void }> {
  let spliced: Record<string, unknown> = { session_expires_at: inMinutes(2) };
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...(await response.json()), ...spliced } });
  });
  await gotoConsole(page);
  await navToRoute(page, "/admin/providers");
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).click();
  await page.getByTestId("provider-row-github").locator("textarea").fill(BASE_URL);
  await expect(page.getByText(EXPIRING_SOON)).toBeVisible();
  await page.evaluate(() => {
    (window as unknown as { beforeRenew?: boolean }).beforeRenew = true;
  });
  return {
    answer: (over) => {
      spliced = { ...spliced, ...over };
    },
  };
}
const sameDocument = (page: Page) =>
  page.evaluate(() => (window as unknown as { beforeRenew?: boolean }).beforeRenew ?? false);

test.describe("the expiry banner: sign in again in place", () => {
  test("the same person signing in keeps the tab and the draft, and the expiry moved", async ({ page }) => {
    const { answer } = await draftUnderAnExpiringSession(page);
    const opened = page.waitForEvent("popup");
    await page.getByRole("button", { name: REAUTH_RENEW.CTA }).click();
    const popup = await opened;

    // The strip takes the banner's place — room for a cold lazy chunk.
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText(EXPIRING_SOON)).toHaveCount(0);
    await expect(page.getByRole("button", { name: REAUTH_RENEW.CANCEL })).toBeFocused();
    // The old session answering /me is not a renewal: the strip keeps waiting.
    await page.waitForResponse((r) => isMe(r.url()));
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toBeVisible();

    const later = inMinutes(60);
    answer({ session_expires_at: later });
    const until = await page.evaluate(
      (iso) => new Date(iso).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" }),
      later,
    );
    await expect(page.getByText(REAUTH_RENEW.RENEWED(until))).toBeVisible();
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toHaveCount(0);
    await expect(page.getByText(EXPIRING_SOON)).toHaveCount(0);
    // The page never left: same document, same route, the draft as typed.
    expect(await sameDocument(page)).toBe(true);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByTestId("provider-row-github").locator("textarea")).toHaveValue(BASE_URL);
    await expect.poll(() => popup.isClosed()).toBe(true);
  });

  test("someone else signing in from the banner reloads the page as them — the draft is gone", async ({ page }) => {
    const { answer } = await draftUnderAnExpiringSession(page);
    await page.getByRole("button", { name: REAUTH_RENEW.CTA }).click();
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toBeVisible({ timeout: 15_000 });

    // Their session ends when the first person's did: who it is decides, not
    // the expiry, which never moved.
    answer({ principal: "someone-else" });
    await expect.poll(() => sameDocument(page)).toBe(false);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toHaveCount(0);
  });

  test("after Cancel, someone else finishing that sign-in still reloads the page as them", async ({ page }) => {
    const { answer } = await draftUnderAnExpiringSession(page);
    const opened = page.waitForEvent("popup");
    await page.getByRole("button", { name: REAUTH_RENEW.CTA }).click();
    const popup = await opened;
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toBeVisible({ timeout: 15_000 });

    // Cancel: the window closes and the banner is back, on the same page.
    await page.getByRole("button", { name: REAUTH_RENEW.CANCEL }).click();
    await expect(page.getByText(EXPIRING_SOON)).toBeVisible();
    await expect(page.getByText(REAUTH_DIALOG.WAITING)).toHaveCount(0);
    await expect.poll(() => popup.isClosed()).toBe(true);
    // /me is still read, and the same person answering it changes nothing.
    await page.waitForResponse((r) => isMe(r.url()));
    await page.waitForResponse((r) => isMe(r.url()));
    expect(await sameDocument(page)).toBe(true);
    await expect(page.getByTestId("provider-row-github").locator("textarea")).toHaveValue(BASE_URL);

    // The sign-in that window began lands after all, as someone else.
    answer({ principal: "someone-else" });
    await expect.poll(() => sameDocument(page)).toBe(false);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
  });
});
