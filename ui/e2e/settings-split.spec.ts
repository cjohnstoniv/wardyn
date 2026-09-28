/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, asRealSecurityAdmin, expandCard, gotoConsole, navToRoute } from "./fixtures";

// E2E coverage for the M-5 settings split (#636):
// src/app/components/screens/settings/{admin-settings-screen,your-account-screen,admin-ssh-keys-card}.tsx
//
// Settings' own Host/Model-provider round trips are settings-connections
// .spec.ts's job; ssh-keys.spec.ts and ssh-keys-capped.spec.ts cover Your
// account's SSH keys pane. This file proves the three things specific to the
// split itself: a super admin's new Admin SSH keys card (S-1), a security
// admin's refusal at /admin/settings with nothing fetched (S-5), and a
// member's Your account carrying no admin card at all.
//
// #1200 compact cards: every card collapses to a one-line summary by default
// and expands on click — the Admin SSH keys test below expands it before
// reading its body.

test.describe("Admin Settings — Admin SSH keys (S-1)", () => {
  test("a super admin sees the Admin SSH keys card, empty by default", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");

    const card = page
      .locator("section")
      .filter({ has: page.getByRole("heading", { name: "Admin SSH keys" }) });
    await expect(card).toBeVisible();
    await expandCard(page, "Admin SSH keys");
    await expect(card.getByText("No admin keys")).toBeVisible();

    // The reused add dialog — same one Your account's "+ Add key" opens.
    await card.getByRole("button", { name: "Add key" }).click();
    const addDialog = page.getByRole("dialog");
    await expect(addDialog.getByRole("heading", { name: "Add key" })).toBeVisible();
    await addDialog.getByRole("button", { name: "Cancel" }).click();
    await expect(addDialog).toHaveCount(0);
  });
});

test.describe("Admin Settings — S-5, a security admin is refused", () => {
  test("a security admin at /admin/settings gets the refusal, and nothing behind it loads", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");

    // The app SHELL polls GET /setup/status on every route (App.tsx's own
    // RequireSetup/ModelAccessProvider state) — that fires regardless of
    // page or role, so it is not this test's claim. What S-5 promises is
    // narrower: navigating INTO Settings adds no further read of its own —
    // neither a second /setup/status round trip nor GET /site-config (which
    // only AdminSettingsScreen ever calls, gated on `adminReads`). Counting
    // from here, right before the navigation, isolates that claim.
    let setupStatusCalls = 0;
    await page.route("**/api/v1/setup/status", (route) => {
      setupStatusCalls++;
      return route.continue();
    });
    const siteConfigCalls: string[] = [];
    await page.route("**/api/v1/site-config", (route) => {
      siteConfigCalls.push(route.request().url());
      return route.continue();
    });

    await navToRoute(page, "/admin/settings");

    await expect(page.getByRole("heading", { name: "Admin view" })).toBeVisible();
    await expect(page.getByText("Settings is for super admins. You're signed in as a security admin.")).toBeVisible();
    const back = page.getByRole("button", { name: "Back to Runs" });
    await expect(back).toBeVisible();
    expect(setupStatusCalls, "navigating into Settings must add no further GET /setup/status").toBe(0);
    expect(siteConfigCalls, "GET /site-config must not fire for a refused caller").toEqual([]);

    await back.click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
  });
});

test.describe("Your account — no admin cards for a member", () => {
  test("a member's Your account shows its own cards, and none of Admin Settings'", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");

    await expect(page.getByRole("heading", { name: "Your account", level: 1 })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Model provider" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 3 })).toBeVisible();

    for (const title of ["Host", "Model providers", "Workspace providers", "User drives", "Admin SSH keys"]) {
      await expect(page.getByRole("heading", { name: title })).toHaveCount(0);
    }
  });
});
