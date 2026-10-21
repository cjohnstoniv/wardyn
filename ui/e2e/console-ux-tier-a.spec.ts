/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier A: the pins that need a real layout engine (390px widths and
// the light theme's computed colours). Everything text- or class-shaped is pinned in
// vitest next to its screen.
import { randomUUID } from "node:crypto";
import type { Locator, Page } from "@playwright/test";
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

// Load the console at the default viewport (at 390px the sidebar's "Runs" link that gotoConsole waits
// for is not rendered), then shrink to a phone and client-side navigate, as providers.spec.ts does.
async function phoneAt(page: Page, view: "user" | "admin", path: string): Promise<void> {
  await gotoConsole(page, view);
  await page.setViewportSize(PHONE);
  await navToRoute(page, path);
}

async function boxOf(target: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await target.boundingBox();
  if (!box) throw new Error("element has no layout box");
  return box;
}

test.describe("Tier A at 390px", () => {
  // #2839, #2840: the tier matrix scrolls inside its own box (its sr-only labels no longer
  // widen the page), and without a runner it is dimmed by its cells alone, not twice over.
  test("Environment step: the page does not scroll sideways and the matrix is not dimmed", async ({ page }) => {
    await phoneAt(page, "admin", "/admin/setup?step=environment");
    const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
    await expect(matrix).toBeVisible();
    expect(await scrollsSideways(page)).toBe(false);
    await expect(matrix).toHaveCSS("opacity", "1");
    await expect(matrix).toHaveCSS("pointer-events", "auto");
  });

  // #2850: the event counter drops to its own line whole instead of wrapping inside itself and clipping.
  test("Audit: the event counter sits on one line inside the viewport", async ({ page }) => {
    await phoneAt(page, "admin", "/admin/audit");
    const counter = page.getByText(/^\d+ events?$/);
    await expect(counter).toBeVisible();
    const box = await boxOf(counter);
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
    expect(box.height).toBeLessThan(26);
  });

  // #2849: an event row is at most two lines, fits the viewport, keeps a readable description and a short run chip.
  test("Audit: rows fit the viewport in three lines at most, the description is readable and the run chip is short", async ({ page }) => {
    await gotoConsole(page, "admin");
    const id = await seedRun(page);
    await page.setViewportSize(PHONE);
    await navToRoute(page, "/admin/audit");
    const chip = page.locator(`main [data-testid="run-ref"] button[title^="${id}"]`).first(); // #3036: the short id inside the run reference
    await expect(chip).toBeVisible();
    const chipBox = await boxOf(chip);
    expect(chipBox.x + chipBox.width).toBeLessThanOrEqual(PHONE.width);
    expect((await chip.innerText()).length).toBeLessThanOrEqual(8);
    const row = chip.locator("xpath=../..");
    const description = row.getByText(/^Created the run/).first();
    expect((await boxOf(description)).width).toBeGreaterThan(150);
    // Count the row's visual lines: its children's tops, grouped within 8px.
    const lines = await row.evaluate((el) => {
      const tops = [...el.children].map((c) => c.getBoundingClientRect().top).sort((a, b) => a - b);
      return tops.filter((t, i) => i === 0 || t - tops[i - 1] > 8).length;
    });
    // #3036: the run reference (title, then the short id) no longer fits beside the description at 390px, so it takes a third line.
    expect(lines).toBeLessThanOrEqual(3);
    expect(await scrollsSideways(page)).toBe(false);
  });

  // #2870: the tab set never wraps a label and stays inside its card.
  test("Providers: the tab bar does not wrap or leave the card", async ({ page }) => {
    await phoneAt(page, "admin", "/admin/providers");
    const git = page.getByRole("button", { name: /^Git providers/ });
    const storage = page.getByRole("button", { name: /^Storage/ });
    await expect(git).toBeVisible();
    const [g, st] = [await boxOf(git), await boxOf(storage)];
    expect(g.height).toBeCloseTo(st.height, 0);
    const barBox = await boxOf(git.locator("xpath=.."));
    expect(barBox.x + barBox.width).toBeLessThanOrEqual(PHONE.width);
    expect(await scrollsSideways(page)).toBe(false);
  });

  // #2896: the four-tab strip scrolls inside itself, so choosing Changes does not shift the page.
  test("Governance: the tab strip stays inside the viewport on every tab", async ({ page }) => {
    await phoneAt(page, "admin", "/admin/governance");
    const strip = page.getByRole("tablist");
    await expect(strip).toBeVisible();
    const box = await boxOf(strip);
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
    expect(await scrollsSideways(page)).toBe(false);
    await page.getByRole("tab", { name: /^Changes/ }).click();
    await expect(page).toHaveURL(/tab=changes/);
    expect(await scrollsSideways(page)).toBe(false);
    expect((await boxOf(page.getByRole("heading", { level: 1 }))).x).toBeGreaterThanOrEqual(0);
  });

  // #2909 (stopgap): the Overview grid keeps its 480px floor but its own scroll box scrolls sideways, never the page.
  test("Run detail Overview: the canvas pane scrolls sideways, the page does not", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await page.setViewportSize(PHONE);
    await navToRoute(page, `/runs/${id}`);
    const pane = page.getByTestId("run-canvas-scroll");
    await expect(pane).toBeVisible();
    await expect(pane).toHaveCSS("overflow-x", "auto");
    expect(await pane.evaluate((n) => n.scrollWidth > n.clientWidth + 1)).toBe(true);
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

    // #2910: the pane is near-black in both themes, so its frame must resolve the DARK --card token.
    // (Computed colours are not usable here: Chromium reports color-mix() as oklab in both themes.)
    test("the terminal pane resolves the dark tokens", async ({ page }) => {
      await gotoConsole(page);
      const id = await seedRun(page);
      await navToRoute(page, `/runs/${id}`);
      const frame = page.getByTestId("run-pane-frame").first();
      await expect(frame).toBeVisible();
      const card = await frame.evaluate((n) => getComputedStyle(n).getPropertyValue("--card").trim());
      // The token may be written #111 or #111111; read either as three channels.
      const hex = /^#([0-9a-f]{1,2})([0-9a-f]{1,2})([0-9a-f]{1,2})$/i.exec(card);
      expect(hex, `--card was "${card}"`).not.toBeNull();
      const [r, g, b] = [hex![1], hex![2], hex![3]].map((h) => parseInt(h.length === 1 ? h + h : h, 16));
      expect((r + g + b) / 3).toBeLessThan(60);
    });
  });
}
