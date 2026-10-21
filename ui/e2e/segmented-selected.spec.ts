/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2832: the selected option of a Segmented control is tellable at a glance in both themes, and a dirty chip on it
// stays legible. Needs a real layout engine's computed colours; the classes are pinned in segmented.test.tsx.
import type { Locator } from "@playwright/test";
import { test, expect, expandCard, gotoConsole, navTo, navToRoute } from "./fixtures";
import { PERM } from "../src/app/lib/permissions-copy";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";

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

    test("a dirty chip on the selected option reads at 4.5:1 or better", async ({ page }) => {
      await gotoConsole(page);
      await navToRoute(page, "/admin/settings");
      const card = page.getByTestId("providers-card");
      await expect(card).toBeVisible();
      await expandCard(page, "Workspace providers");
      await card.getByText(PROVIDERS.CARD_OPEN).click();
      // Adding a row makes the Git draft differ from what loaded, which puts the dirty chip on the selected Git tab.
      await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).first().click();
      const chip = page.getByTestId("tab-dirty-chip-git").locator(":scope > span");
      await expect(chip).toBeVisible();
      await expect(page.getByTestId("tab-dirty-chip-git").locator("xpath=ancestor::button")).toHaveAttribute("aria-pressed", "true");
      const [text, bg] = await Promise.all([chip.evaluate((el) => getComputedStyle(el).color), chip.evaluate((el) => getComputedStyle(el).backgroundColor)]);
      expect(contrast(text, bg)).toBeGreaterThanOrEqual(4.5);
    });
  });
}
