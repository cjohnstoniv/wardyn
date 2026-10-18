/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier A: the pins that need a real layout engine (390px widths and
// the light theme's computed colours). Everything text- or class-shaped is pinned in
// vitest next to its screen.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";

const PHONE = { width: 390, height: 844 };

// True when the page can be scrolled sideways: the document or the shell's <main> is wider than its box.
async function scrollsSideways(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const root = document.documentElement;
    const main = document.querySelector("main");
    return root.scrollWidth > root.clientWidth + 1 || (!!main && main.scrollWidth > main.clientWidth + 1);
  });
}

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "tier-a e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Tier A at 390px", () => {
  test.use({ viewport: PHONE });

  // #2839, #2840: the tier matrix scrolls inside its own box (its sr-only labels no longer
  // widen the page), and without a runner it is dimmed by its cells alone, not twice over.
  test("Environment step: the page does not scroll sideways and the matrix is not dimmed", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/setup?step=environment");
    const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
    await expect(matrix).toBeVisible();
    expect(await scrollsSideways(page)).toBe(false);
    await expect(matrix).toHaveCSS("opacity", "1");
    await expect(matrix).toHaveCSS("pointer-events", "auto");
  });

  // #2850: the event counter drops to its own line whole instead of wrapping inside itself and clipping.
  test("Audit: the event counter sits on one line inside the viewport", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/audit");
    const counter = page.getByText(/^\d+ events?$/);
    await expect(counter).toBeVisible();
    const box = (await counter.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
    expect(box.height).toBeLessThan(26);
  });

  // #2849: an event row wraps instead of overflowing; the description keeps real width and the run chip is short.
  test("Audit: rows fit the viewport, the description is readable and the run chip is short", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/audit");
    const chip = page.locator("main button.font-mono").first();
    await expect(chip).toBeVisible();
    const chipBox = (await chip.boundingBox())!;
    expect(chipBox.x + chipBox.width).toBeLessThanOrEqual(PHONE.width);
    expect(((await chip.innerText()) ?? "").length).toBeLessThanOrEqual(8);
    const desc = chip.locator("xpath=ancestor::div[contains(@class,'flex-wrap')][1]").locator("span.truncate").first();
    expect((await desc.boundingBox())!.width).toBeGreaterThan(150);
    expect(await scrollsSideways(page)).toBe(false);
  });

  // #2870: the tab set never wraps a label and stays inside its card.
  test("Providers: the tab bar does not wrap or leave the card", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/providers");
    const git = page.getByRole("button", { name: /^Git providers/ });
    const storage = page.getByRole("button", { name: /^Storage/ });
    await expect(git).toBeVisible();
    const [g, st] = [(await git.boundingBox())!, (await storage.boundingBox())!];
    expect(g.height).toBeCloseTo(st.height, 0);
    const bar = git.locator("xpath=..");
    expect((await bar.boundingBox())!.x + (await bar.boundingBox())!.width).toBeLessThanOrEqual(PHONE.width);
    expect(await scrollsSideways(page)).toBe(false);
  });

  // #2896: the four-tab strip scrolls inside itself, so choosing Changes does not shift the page.
  test("Governance: the tab strip stays inside the viewport on every tab", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/governance");
    const strip = page.getByRole("tablist");
    await expect(strip).toBeVisible();
    const box = (await strip.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
    expect(await scrollsSideways(page)).toBe(false);
    await page.getByRole("tab", { name: /^Changes/ }).click();
    await expect(page).toHaveURL(/tab=changes/);
    expect(await scrollsSideways(page)).toBe(false);
    expect((await page.getByRole("heading", { level: 1 }).boundingBox())!.x).toBeGreaterThanOrEqual(0);
  });

  // #2909 (stopgap): the Overview grid keeps its 480px floor but scrolls inside its pane, never the page.
  test("Run detail Overview: the grid pane scrolls sideways, the page does not", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await navToRoute(page, `/runs/${id}`);
    await expect(page.getByRole("tab", { name: /Overview/ })).toHaveAttribute("data-state", "active");
    await expect
      .poll(() =>
        page.evaluate(() =>
          [...document.querySelectorAll<HTMLElement>(".scroll-thin")].some(
            (n) => getComputedStyle(n).overflowX === "auto" && n.scrollWidth > n.clientWidth + 1,
          ),
        ),
      )
      .toBe(true);
    expect(await scrollsSideways(page)).toBe(false);
  });
});

for (const theme of ["light", "dark"] as const) {
  test.describe(`run detail, ${theme} theme`, () => {
    test.beforeEach(async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem("wardyn-theme", t), theme);
    });

    // #2911: the selected tab is visibly filled and differs from an unselected one.
    test("the selected tab is visibly different from the others", async ({ page }) => {
      await gotoConsole(page);
      const id = await seedRun(page);
      await navToRoute(page, `/runs/${id}`);
      const selected = page.getByRole("tab", { name: /Overview/ });
      await expect(selected).toHaveAttribute("data-state", "active");
      const other = page.getByRole("tab", { name: /Audit/ });
      const style = (el: typeof selected) =>
        el.evaluate((n) => {
          const s = getComputedStyle(n);
          return { bg: s.backgroundColor, color: s.color };
        });
      const [on, off] = [await style(selected), await style(other)];
      expect(on.bg).not.toBe("rgba(0, 0, 0, 0)");
      expect(on.bg).not.toBe(off.bg);
      expect(on.color).not.toBe(off.color);
    });

    // #2910: the pane is near-black in both themes, so its header must be dark with light text.
    test("the terminal pane header stays dark", async ({ page }) => {
      await gotoConsole(page);
      const id = await seedRun(page);
      await navToRoute(page, `/runs/${id}`);
      const header = page.locator('div.dark[class*="0d1117"] > div').first();
      await expect(header).toBeVisible();
      const [r, g, b] = await header.evaluate((n) => (getComputedStyle(n).backgroundColor.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number));
      expect((r + g + b) / 3).toBeLessThan(90);
    });
  });
}
