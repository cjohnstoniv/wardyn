/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { SETTINGS_GROUPS } from "../src/app/components/wardyn/copy/card-status";

// Admin Settings reads as three places, and the two cards that only link out are rows with an arrow (#3048).
test("Settings groups its cards, and Workspace providers and User drives are link rows", async ({ page }) => {
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/settings");

  for (const [group, testIds] of [
    [SETTINGS_GROUPS.HOST, ["host-card"]],
    [SETTINGS_GROUPS.CONNECTIONS, ["model-providers-list", "providers-card", "user-drives-card"]],
    [SETTINGS_GROUPS.PEOPLE, ["approval-notify-card", "scim-card"]],
  ] as const) {
    const section = page.getByRole("region", { name: group });
    await expect(section).toBeVisible();
    for (const id of testIds) await expect(section.getByTestId(id)).toBeVisible();
  }

  await page.getByTestId("providers-card").getByRole("link").click();
  await expect(page).toHaveURL(/\/admin\/providers$/);
  await navToRoute(page, "/admin/settings");
  await page.getByTestId("user-drives-card").getByRole("link").click();
  await expect(page).toHaveURL(/\/admin\/drives$/);
});
