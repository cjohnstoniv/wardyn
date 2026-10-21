/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2832: the selected option of a Segmented control is tellable at a glance in both themes. Needs a real layout engine's
// computed colours; the classes, including the dirty chip's solid card fill on the selected option, are pinned in segmented.test.tsx
// (no screen sets a Segmented option dirty now that the providers sections are PageTabs, so no page reaches that chip).
import type { Locator } from "@playwright/test";
import { test, expect, gotoConsole, navTo } from "./fixtures";
import { PERM } from "../src/app/lib/permissions-copy";

function channels(rgb: string): number[] {
  return rgb.match(/\d+(\.\d+)?/g)!.slice(0, 3).map(Number);
}

function luminance(rgb: string): number {
  const c = channels(rgb).map((v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// The colour a transparent element is actually drawn on: the first ancestor (or itself) with a painted background.
function paintedBg(el: Locator): Promise<string> {
  return el.evaluate((node) => {
    for (let e: Element | null = node; e; e = e.parentElement) {
      const bg = getComputedStyle(e).backgroundColor;
      const alpha = bg.startsWith("rgba") ? Number(bg.match(/[\d.]+/g)![3]) : 1;
      if (alpha > 0) return bg;
    }
    return "rgb(255, 255, 255)";
  });
}

for (const theme of ["light", "dark"] as const) {
  test.describe(`Segmented (${theme})`, () => {
    test.beforeEach(async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem("wardyn-theme", t), theme);
    });

    test("the selected option's fill clears 3:1 against the unselected cell it sits beside", async ({ page }) => {
      await gotoConsole(page, "admin");
      await navTo(page, "Governance");
      const selected = page.getByRole("button", { name: PERM.SUBJECT_GROUP, exact: true });
      const unselected = page.getByRole("button", { name: PERM.SUBJECT_USER, exact: true });
      await expect(selected).toHaveAttribute("aria-pressed", "true");
      await expect(unselected).toHaveAttribute("aria-pressed", "false");
      const fill = await selected.evaluate((el) => getComputedStyle(el).backgroundColor);
      expect(contrast(fill, await paintedBg(unselected))).toBeGreaterThanOrEqual(3);
    });
  });
}
