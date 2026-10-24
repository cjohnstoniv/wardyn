/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";

// #2884: an address the console does not know used to redirect to Runs and lose the address. It now keeps the
// address and says there is no page there; Back to Runs goes to the view the address belongs to.
test.use({ viewport: { width: 1440, height: 900 } });

const card = (page: import("@playwright/test").Page) => page.getByRole("region", { name: "Page not found" });

test("an unknown address shows Page not found at that address, not Runs", async ({ page }) => {
  await gotoConsole(page);
  await page.goto("/definitely/not/a/page");
  await expect(card(page)).toBeVisible();
  await expect(card(page).getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expect(card(page)).toContainText("There's no page at /definitely/not/a/page.");
  await expect(card(page).getByText("Not found", { exact: true })).toBeVisible();
  expect(new URL(page.url()).pathname).toBe("/definitely/not/a/page");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toHaveCount(0);
  await card(page).getByRole("button", { name: "Back to Runs" }).click();
  await expect(page).toHaveURL(/\/runs$/);
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
});

test("an unknown Admin address does the same and goes back to Admin Runs", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/definitely-not");
  await expect(card(page)).toContainText("There's no page at /admin/definitely-not.");
  expect(new URL(page.url()).pathname).toBe("/admin/definitely-not");
  await card(page).getByRole("button", { name: "Back to Runs" }).click();
  await expect(page).toHaveURL(/\/admin\/runs$/);
});

test("an address M-1b removed shows the card too", async ({ page }) => {
  await gotoConsole(page);
  await page.goto("/integrations");
  await expect(card(page)).toContainText("There's no page at /integrations.");
});
