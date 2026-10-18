/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";

// A host that reports no barrier at all (#2571, #2574). jsdom proves each
// control's disabled state; this walk proves the person's path: the funnel
// reaches Review on such a host, and neither exit from it lets setup be
// finished or handed off while nothing confines a run. The Environment step
// must also stop promising a browser-saved default that no code writes.

async function mockNoBarrier(page: Page): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.onboarding_complete = false;
    json.runner = { driver: "docker", confinement_classes: [], confinement_substrates: {} };
    await route.fulfill({ response, json });
  });
  await page.addInitScript(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode — ignore */
    }
  });
}

test.describe("Setup on a host with no barrier", () => {
  test("Environment promises no browser-saved default (#2574)", async ({ page }) => {
    await mockNoBarrier(page);
    await page.goto("/admin/setup");
    await expect(page.getByRole("heading", { name: /pick your barrier/i })).toBeVisible({ timeout: 15_000 });
    // The stale promise is gone from the whole page, not just one sentence.
    await expect(page.getByText(/saved in this browser/i)).toHaveCount(0);
    await expect(page.getByText(/save it in this browser/i)).toHaveCount(0);
  });

  test("Review offers neither Finish setup nor Switch to user view (#2571)", async ({ page }) => {
    await mockNoBarrier(page);
    await page.goto("/admin/setup");
    const rail = page.getByRole("navigation", { name: /setup steps/i }).last();
    await expect(rail).toBeVisible({ timeout: 15_000 });
    // Review sits behind the Network gate; the e2e backend's `-runner none`
    // answers no_runner, the one honest bypass (same walk as setup-gate.spec.ts).
    await rail.getByRole("button", { name: /^Network/ }).click();
    await page.getByRole("button", { name: /^Test connectivity$/i }).click();
    await expect(page.getByRole("button", { name: /^Next:/i })).toBeEnabled();
    await rail.getByRole("button", { name: /^Review/ }).click();
    await expect(page.getByRole("heading", { name: /review readiness/i })).toBeVisible();

    await expect(page.getByRole("button", { name: "Switch to user view" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Finish setup" })).toBeDisabled();
  });
});
