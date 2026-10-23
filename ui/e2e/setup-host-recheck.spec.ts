/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";

// #2847: the full-width "Host status" bar showed no status, and Network carried a second Re-check. The one
// Re-check now sits at the right end of the step's eyebrow row, on every step, with the in-flight text beside it.
test.use({ colorScheme: "dark", viewport: { width: 1440, height: 900 } });

for (const [step, eyebrow] of [
  ["Network", /^Step \d of \d/],
  ["Secrets", /Not required before a run can launch/],
] as const) {
  test(`${step}: one Re-check in the step's eyebrow row, and no Host status bar`, async ({ page }) => {
    await gotoConsole(page, "admin");
    await page.goto("/admin/setup?step=corp_network");
    if (step === "Secrets") {
      // Optional steps sit behind the Network gate; the e2e backend runs no runner, the one honest bypass.
      await page.getByRole("button", { name: "Test connectivity" }).click();
      await expect(page.getByRole("button", { name: /^Next:/ })).toBeEnabled();
      await page.getByRole("navigation", { name: "Setup steps" }).getByRole("button", { name: /^Secrets/ }).click();
    }
    const recheck = page.locator("main").getByRole("button", { name: "Re-check", exact: true });
    await expect(recheck).toHaveCount(1);
    await expect(page.getByText("Host status", { exact: true })).toHaveCount(0);
    const label = page.locator("main").locator(".label-eyebrow").filter({ hasText: eyebrow });
    await expect(label).toBeVisible();
    const [r, l] = [(await recheck.boundingBox())!, (await label.boundingBox())!];
    expect(Math.abs(r.y - l.y)).toBeLessThan(30);
    expect(r.x).toBeGreaterThan(l.x + l.width);
  });
}

test("Review carries no second Re-check or stamp of its own", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/setup?step=corp_network");
  // Review sits behind the Network gate; the e2e backend runs no runner, the one honest bypass.
  await page.getByRole("button", { name: "Test connectivity" }).click();
  await expect(page.getByRole("button", { name: /^Next:/ })).toBeEnabled();
  await page.getByRole("navigation", { name: "Setup steps" }).getByRole("button", { name: /^Review/ }).click();
  await expect(page.getByText("Every setup check, in one place.", { exact: false })).toBeVisible();
  await expect(page.locator("main").getByRole("button", { name: "Re-check", exact: true })).toHaveCount(1);
});

test("a forced Re-check shows the in-flight text beside a disabled button", async ({ page }) => {
  await gotoConsole(page, "admin");
  let release!: () => void;
  const held = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (route.request().url().includes("recheck=1")) await held;
    await route.fallback();
  });
  await page.goto("/admin/setup?step=corp_network");
  const recheck = page.locator("main").getByRole("button", { name: "Re-check", exact: true });
  await recheck.click();
  await expect(page.getByText("Checking Wardyn's setup…")).toBeVisible();
  await expect(recheck).toBeDisabled();
  release();
  await expect(page.getByText("Checking Wardyn's setup…")).toHaveCount(0);
  await expect(recheck).toBeEnabled();
});
