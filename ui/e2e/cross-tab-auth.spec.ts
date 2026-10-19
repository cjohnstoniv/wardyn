/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { ADMIN_TOKEN, expect, gotoConsole, sidebarLink, test } from "./fixtures";
import { REAUTH_DIALOG, REAUTH_EXTRA } from "../src/app/lib/reauth-copy";

// DR-0020 — two tabs of one browser share a session, so what one does is seen by the other. The real
// backend, the real BroadcastChannel and the token in localStorage that both tabs read.
// Sign-out in one tab puts the other behind the sign-in dialog, saying why; signing back in there lets
// the other carry on without typing the token again.

test("signed out in one tab, the other says so and carries on once signed back in", async ({ page }) => {
  await gotoConsole(page);
  const other = await page.context().newPage();
  await gotoConsole(other);

  await page.locator('header button[aria-haspopup="menu"]').click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page.locator("#token")).toBeVisible();

  // The other tab asks who is signed in and finds no one: its page stays, and the dialog says why.
  const dialog = other.getByRole("dialog", { name: REAUTH_DIALOG.TITLE });
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  await expect(dialog.getByText(REAUTH_EXTRA.SIGNED_OUT_ELSEWHERE)).toBeVisible();
  // The page behind the dialog is still there (the dialog hides it from the accessibility tree).
  await expect(other.getByRole("link", { name: /^Runs/, includeHidden: true })).toHaveCount(1);

  // Signed back in on the first tab (and remembered on this device): the second needs no token typed.
  await page.locator("#token").fill(ADMIN_TOKEN);
  await page.getByRole("checkbox", { name: "Remember on this device" }).click();
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(sidebarLink(page, "Runs")).toBeVisible();
  await expect(dialog).toBeHidden({ timeout: 15_000 });
  await expect(sidebarLink(other, "Runs")).toBeVisible();
});

// The tab that signed out deliberately shows the sign-in gate; it must still hear another tab sign in, which
// it shows by probing the session again. (A token typed in the other tab is that tab's own, so this tab
// has nothing to carry on with here; a shared cookie session is what lets it in, and the unit test pins that.)
test("the tab that signed out re-checks who is signed in when the other tab signs in", async ({ page }) => {
  await gotoConsole(page);
  const other = await page.context().newPage();
  await gotoConsole(other);

  await page.locator('header button[aria-haspopup="menu"]').click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page.locator("#token")).toBeVisible();

  const dialog = other.getByRole("dialog", { name: REAUTH_DIALOG.TITLE });
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  const asked = page.waitForRequest((r) => new URL(r.url()).pathname === "/api/v1/runs", { timeout: 15_000 });
  await dialog.locator("#reauth-token").fill(ADMIN_TOKEN);
  await dialog.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: 15_000 });
  await asked;
});
