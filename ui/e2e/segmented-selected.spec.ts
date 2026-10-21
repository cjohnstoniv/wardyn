/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2832: the selected option of a Segmented control is tellable at a glance in both themes. Needs a real
// layout engine's computed colours; the class is pinned in segmented.test.tsx.
import { test, expect, gotoConsole, navTo } from "./fixtures";
import { PERM } from "../src/app/lib/governance-copy";

function luminance(rgb: string): number {
  const c = rgb.match(/\d+(\.\d+)?/g)!.slice(0, 3).map(Number).map((v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}

for (const theme of ["light", "dark"] as const) {
  test(`Segmented: the selected option's fill clears 3:1 against the page (${theme})`, async ({ page }) => {
    await page.addInitScript((t) => localStorage.setItem("wardyn-theme", t), theme);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    const selected = page.getByRole("button", { name: PERM.SUBJECT_GROUP, exact: true });
    await expect(selected).toHaveAttribute("aria-pressed", "true");
    const [fill, pageBg] = await Promise.all([
      selected.evaluate((el) => getComputedStyle(el).backgroundColor),
      page.evaluate(() => getComputedStyle(document.body).backgroundColor),
    ]);
    const [a, b] = [luminance(fill), luminance(pageBg)].sort((x, y) => y - x);
    expect((a + 0.05) / (b + 0.05)).toBeGreaterThanOrEqual(3);
  });
}
