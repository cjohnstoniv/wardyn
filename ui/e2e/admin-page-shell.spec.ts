/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navTo, type NavLabel } from "./fixtures";

// The admin page shell (round-6 sheet): every admin page sits in one of two columns, list or form, so its
// title lands on the same x as the rest of its family (#3058), and every page title is the one page-title
// rung, 28 px semibold (#3080). Templates is on the shared PageHeader, without its old eyebrow.

async function title(page: Page, label: NavLabel | "Templates") {
  if (label === "Templates") await page.locator("aside").getByRole("link", { name: /^Templates/ }).click();
  else await navTo(page, label);
  const h1 = page.locator("main").getByRole("heading", { level: 1, name: label, exact: true });
  await expect(h1).toBeVisible();
  const box = await h1.boundingBox();
  const width = await page.locator("[data-page-width]", { has: h1 }).getAttribute("data-page-width");
  const font = await h1.evaluate((el) => {
    const s = getComputedStyle(el);
    return `${s.fontSize} ${s.fontWeight}`;
  });
  return { x: Math.round(box!.x), width, font };
}

test.describe("admin page shell (#3058, #3080)", () => {
  test("list pages share one title x, form pages share another, all on the page-title rung", async ({ page }) => {
    await gotoConsole(page, "admin");
    const lists = [];
    for (const label of ["People", "Credentials", "Governance", "Audit", "Templates"] as const) lists.push(await title(page, label));
    const forms = [];
    for (const label of ["Settings", "Approvals"] as const) forms.push(await title(page, label));

    expect(lists.map((t) => t.width)).toEqual(lists.map(() => "list"));
    expect(forms.map((t) => t.width)).toEqual(forms.map(() => "form"));
    // A centred column moves by half a scrollbar when one page scrolls and another doesn't; nothing more.
    const spread = (xs: number[]) => Math.max(...xs) - Math.min(...xs);
    expect(spread(lists.map((t) => t.x))).toBeLessThanOrEqual(8);
    expect(spread(forms.map((t) => t.x))).toBeLessThanOrEqual(8);
    expect(forms[0].x).toBeGreaterThan(lists[0].x);
    for (const t of [...lists, ...forms]) expect(t.font).toBe("28px 600");
  });

  test("Templates uses the shared page header: no eyebrow, the action on the title row", async ({ page }) => {
    await gotoConsole(page, "admin");
    await page.locator("aside").getByRole("link", { name: /^Templates/ }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Templates" })).toBeVisible();
    await expect(page.getByText("Saved starting points", { exact: true })).toHaveCount(0);
    await expect(page.getByTestId("page-header-actions").getByRole("link", { name: "New run from scratch" })).toBeVisible();
  });
});
