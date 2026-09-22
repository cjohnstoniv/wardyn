/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, Request } from "@playwright/test";
import { ADMIN_TOKEN, expect, gotoConsole, navToRoute, test } from "./fixtures";
import { REAUTH_BAR, REAUTH_DIALOG } from "../src/app/lib/reauth-copy";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";

// #483 — a session that ends mid-page keeps the page. A real screen with a
// typed-but-unsaved draft (/providers, a git row's base URL), a Save whose
// PUT hits the expiry (route interception answers it 401), and the dialog
// that opens over it.

const BASE_URL = "https://github.com/acme-reauth";

const isProvidersPut = (r: Request) => r.method() === "PUT" && new URL(r.url()).pathname === "/api/v1/workspace-providers";

/** On /providers with a git row typed but not saved; the Save then 401s. */
async function saveIntoAnExpiredSession(page: Page): Promise<{ puts: () => number }> {
  let puts = 0;
  page.on("request", (r) => {
    if (isProvidersPut(r)) puts += 1;
  });
  await gotoConsole(page);
  await navToRoute(page, "/providers");
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
