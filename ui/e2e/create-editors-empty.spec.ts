/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { DRIVES, PEOPLE } from "../src/app/lib/user-drives-copy";

// #3052: New profile / New drive on an empty list open the editor in the empty state's place, not under a
// "No ... yet" hero with a greyed New button above it. Cancel brings the empty state back with New enabled.
// Both lists are served empty through page.route: the shared e2e backend seeds profiles and drives for other
// specs, and this suite runs fully parallel.

test.describe("create editors on an empty list (#3052)", () => {
  test("New profile replaces the empty state, and Cancel brings it back", async ({ page }) => {
    await page.route(/\/api\/v1\/governance(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: { units: [], memberships: [] } }) : route.fallback(),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/governance");
    await expect(page.getByRole("heading", { name: GOV.EMPTY_TITLE, level: 4 })).toBeVisible();

    await page.getByRole("button", { name: GOV.NEW_CTA }).click();
    await expect(page.getByTestId("governance-profile-editor")).toBeVisible();
    await expect(page.getByText(GOV.EMPTY_TITLE)).toHaveCount(0);

    await page.getByRole("button", { name: PEOPLE.CANCEL }).click();
    await expect(page.getByTestId("governance-profile-editor")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: GOV.EMPTY_TITLE, level: 4 })).toBeVisible();
    await expect(page.getByRole("button", { name: GOV.NEW_CTA })).toBeEnabled();
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("New drive replaces the empty state, and Cancel brings it back", async ({ page }) => {
    await page.route(/\/api\/v1\/drives(\?|$)/, (route) =>
      route.request().method() === "GET"
        ? route.fulfill({ json: { drives: [], grants: [], runner_target: "docker", host_roots_configured: true, disabled: false } })
        : route.fallback(),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/drives");
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toBeVisible();

    await page.getByRole("button", { name: DRIVES.NEW_CTA }).click();
    await expect(page.getByTestId("drives-drive-editor")).toBeVisible();
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toHaveCount(0);

    await page.getByRole("button", { name: PEOPLE.CANCEL }).click();
    await expect(page.getByTestId("drives-drive-editor")).toHaveCount(0);
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toBeVisible();
    await expect(page.getByRole("button", { name: DRIVES.NEW_CTA })).toBeEnabled();
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });
});
