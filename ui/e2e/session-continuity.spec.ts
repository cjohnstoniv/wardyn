/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo, sidebarLink } from "./fixtures";
import { SESSION_ENDED, SESSION_MENU, signedInUntil } from "../src/app/lib/session-continuity-copy";
import { UNSAVED } from "../src/app/lib/unsaved-copy";
import { USER_TYPES as UT } from "../src/app/lib/user-types-copy";

// #1995 (mock M12): what the console says when a session ends, what it keeps, and what the account menu
// tells the person about the session they hold. The 401 reason rides the cold-load probe, which route
// interception answers; every sentence comes from the copy module, never retyped.

test.describe("session continuity (#1995)", () => {
  test("a cold load whose session was ended on the server says so, by reason", async ({ page }) => {
    await page.route("**/api/v1/runs?limit=1", (route) =>
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "Your session was revoked — sign in again", reason: "revoked_session" }),
      }),
    );
    await page.goto("/");
    const notice = page.getByRole("status").filter({ hasText: SESSION_ENDED.REVOKED });
    await expect(notice).toBeVisible();
    // The sentence for the reason, not the generic one.
    await expect(page.getByText(SESSION_ENDED.EXPIRED)).toHaveCount(0);
  });

  test("a person whose access was turned off is told to ask their admin", async ({ page }) => {
    await page.route("**/api/v1/runs?limit=1", (route) =>
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "Your access to Wardyn has been turned off — ask your Wardyn admin", reason: "identity_deactivated" }),
      }),
    );
    await page.goto("/");
    await expect(page.getByRole("status").filter({ hasText: SESSION_ENDED.DEACTIVATED })).toBeVisible();
  });

  test("an editor that had no guard asks before leaving with typed changes, and keeps them on Keep editing", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "User types");
    await expect(page.getByRole("heading", { name: UT.TITLE, level: 1 })).toBeVisible();
    await page.getByRole("button", { name: UT.NEW_CTA }).click();
    const name = page.locator("#user-type-name");
    await name.fill("draft-type-not-saved");

    await sidebarLink(page, "Runs").click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(UNSAVED.TITLE)).toBeVisible();
    await dialog.getByRole("button", { name: UNSAVED.STAY }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page).toHaveURL(/\/user-types$/);
    await expect(name).toHaveValue("draft-type-not-saved");

    await sidebarLink(page, "Runs").click();
    await page.getByRole("alertdialog").getByRole("button", { name: UNSAVED.DISCARD }).click();
    await expect(page).not.toHaveURL(/\/user-types$/);
  });

  test("the account menu says until when the session lasts, and what Sign out ends", async ({ page }) => {
    const expiry = new Date(Date.now() + 6 * 3600_000);
    await page.route("**/api/v1/me", async (route) => {
      const res = await route.fetch();
      const me = (await res.json()) as Record<string, unknown>;
      await route.fulfill({ response: res, json: { ...me, session_expires_at: expiry.toISOString() } });
    });
    await gotoConsole(page);
    await page.locator('header button[aria-haspopup="menu"]').click();
    const menu = page.getByRole("menu");
    await expect(menu.getByTestId("signed-in-until")).toHaveText(signedInUntil(expiry));
    await expect(menu.getByText(SESSION_MENU.SIGN_OUT_HINT)).toBeVisible();
  });
});
