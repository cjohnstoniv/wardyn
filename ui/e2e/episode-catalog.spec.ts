/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";

// The welcome hero's episode catalog, grouped by deployment path (Shape C,
// approved mock round 2026-08-31). The hero only renders on a
// not-yet-onboarded install, and the harness deliberately seeds itself
// onboarded — so the un-onboarded state is forced the same way
// setup-gate.spec.ts forces it: ride the real /setup/status response and flip
// what the scenario needs (the bypass-seam rule: the seam that skips a
// behavior obligates the spec that covers it).

async function mockFreshInstall(page: Page, opts: { sso?: boolean } = {}): Promise<void> {
  await page.route("**/api/v1/setup/status", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.onboarding_complete = false;
    if (opts.sso) json.auth = { ...json.auth, mode: "sso" };
    await route.fulfill({ response, json });
  });
}

test.describe("episode catalog — Shape C path grouping", () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("single-user install: core leads, single-user path is the deployment group, multi collapses", async ({
    page,
  }) => {
    await mockFreshInstall(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByText("All episodes")).toBeVisible();
    await expect(page.getByText("Start here")).toBeVisible();
    await expect(page.getByText("Your deployment — single-user")).toBeVisible();
    await expect(page.getByText("Running work — any deployment")).toBeVisible();
    await expect(page.getByText("Your deployment — multi-user")).toHaveCount(0);
    // The other path is reachable behind a disclosure, with an honest count.
    const disclosure = page.getByText(/^The multi-user path — \d+ episodes$/);
    await expect(disclosure).toBeVisible();
    await disclosure.click();
    await expect(page.getByText("One command to a cluster")).toBeVisible();
  });

  test("multi-user (SSO) install: the deployment group swaps and member rows are chipped", async ({
    page,
  }) => {
    await mockFreshInstall(page, { sso: true });
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByText("Your deployment — multi-user")).toBeVisible();
    await expect(page.getByText("Your deployment — single-user")).toHaveCount(0);
    await expect(page.getByText(/^The single-user path — \d+ episodes$/)).toBeVisible();
    // 04b + 13: the multi path's member-audience episodes carry the chip.
    await expect(page.getByText("For your members")).toHaveCount(2);
  });
});
