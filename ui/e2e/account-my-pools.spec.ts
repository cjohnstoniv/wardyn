/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, gotoConsole, navToRoute } from "./fixtures";
import { MY_POOLS } from "../src/app/components/wardyn/copy/runner-pools";

// FUG-L404, against the real server: the harness has runners off for the organization, so My runner is
// not on a member's Your account. My pools is, as a card of its own, and the link New Run has always
// used for it (/account#my-runner) opens it.
test.describe("Your account: My pools with runners off", () => {
  test("is its own card, with My runner gone, and expands to Create a pool", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");
    await expect(page.getByRole("heading", { name: "Your account", level: 1 })).toBeVisible();
    const card = page.getByRole("button", { name: new RegExp(`^${MY_POOLS.TITLE}\\s*${MY_POOLS.SUMMARY_NONE}`) });
    await expect(card).toBeVisible();
    await expect(page.getByRole("button", { name: /^My runner/ })).toHaveCount(0);
    await expect(card).toHaveAttribute("aria-expanded", "false");
    await card.click();
    await expect(page.getByText(MY_POOLS.EMPTY)).toBeVisible();
    await expect(page.getByRole("button", { name: MY_POOLS.CREATE })).toBeVisible();
  });

  test("the #my-runner link opens it", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await page.goto("/account#my-runner");
    await expect(page.getByRole("button", { name: new RegExp(`^${MY_POOLS.TITLE}`) })).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByText(MY_POOLS.EMPTY)).toBeVisible();
  });
});
