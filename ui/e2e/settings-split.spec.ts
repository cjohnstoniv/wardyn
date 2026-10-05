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
    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 3 })).toBeVisible();

    for (const title of ["Host", "Model provider", "Model providers", "Workspace providers", "User drives", "Admin SSH keys"]) {
      await expect(page.getByRole("heading", { name: title })).toHaveCount(0);
    }
  });
});

// notify-e4 (packet M6 S3): the read-only Approval notifications card. The e2e
// backend has no WARDYN_APPROVAL_NOTIFY, so the configured case stubs the
// status read; the unconfigured case is the real backend's own answer.
test.describe("Admin Settings — Approval notifications (notify-e4)", () => {
  test("unconfigured, the card says Not set up and names the setting", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    const card = page
      .locator("section")
      .filter({ has: page.getByRole("heading", { name: /^Approval notifications/ }) });
    await expect(card.getByText("Not set up")).toBeVisible();
    await expandCard(page, "Approval notifications");
    await expect(card.getByText("WARDYN_APPROVAL_NOTIFY")).toBeVisible();
    await expect(card.getByRole("table")).toHaveCount(0);
  });

  test("with channels, it shows each one's health and no buttons", async ({ page }) => {
    const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
    await page.route("**/api/v1/approval-notify/status", (route) =>
      route.fulfill({
        json: {
          channels: [
            { id: "sec-oncall", type: "slack", destination_host: "hooks.slack.com", last_success_at: ago(4 * 60_000), failed_last_hour: 0 },
            {
              id: "platform", type: "webhook", destination_host: "hooks.example.com",
              last_error: "http_status:503", last_error_at: ago(6 * 60_000), failed_last_hour: 4,
            },
          ],
        },
      }),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    const card = page
      .locator("section")
      .filter({ has: page.getByRole("heading", { name: /^Approval notifications/ }) });
    await expect(card.getByText("2 channels · 1 failed in the last hour")).toBeVisible();
    await expandCard(page, "Approval notifications");

    const good = card.getByRole("row").filter({ hasText: "sec-oncall" });
    await expect(good.getByText("Slack", { exact: true })).toBeVisible();
    await expect(good.getByText("hooks.slack.com")).toBeVisible();
    await expect(good.getByText("4m ago")).toBeVisible();
    await expect(good.getByText("None", { exact: true })).toBeVisible();
    const bad = card.getByRole("row").filter({ hasText: "platform" });
    await expect(bad.getByText("Never", { exact: true })).toBeVisible();
    await expect(bad.getByText("http_status:503")).toBeVisible();
    await expect(bad.getByText("6m ago")).toBeVisible();
    await expect(card.getByText("approval.notify.failed")).toBeVisible();
    await expect(card.getByRole("button")).toHaveCount(1); // the card's own expand toggle only
  });
});
