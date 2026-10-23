/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";

// #2844: at 1024 the setup step rail was a 56px column of bare dots whose names lived in a title. The full rail
// (names, the Required and Optional setup headings, badges) now shows from lg up, in a 200px column.
test.use({ colorScheme: "dark", viewport: { width: 1024, height: 768 } });

test("at 1024 the setup rail names its steps and where Optional setup begins", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/setup");
  const rail = page.getByRole("navigation", { name: "Setup steps" });
  await expect(rail).toHaveCount(1);
  await expect(rail.getByText("Required", { exact: true })).toBeVisible();
  await expect(rail.getByText("Optional setup", { exact: true })).toBeVisible();
  await expect(rail.getByRole("button", { name: /^Network/ }).getByText("Network", { exact: true })).toBeVisible();
  // The rail's column, not the 56px icon one: wide enough to read a step name.
  const width = (await rail.boundingBox())!.width;
  expect(width).toBeGreaterThan(150);
  expect(width).toBeLessThanOrEqual(200);
});
