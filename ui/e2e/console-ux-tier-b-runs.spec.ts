/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B, run and setup screens: the pins that need a real layout engine (widths,
// wrapping, computed positions). Text-shaped fixes are pinned in vitest next to their screen.
import { randomUUID } from "node:crypto";
import type { Locator, Page } from "@playwright/test";
import { test, expect, asRealMember, consoleAPI, gotoConsole, navToRoute, sidebarLink } from "./fixtures";

const PHONE = { width: 390, height: 844 };

// Load the console at the default viewport (at 390px the sidebar's "Runs" link that gotoConsole waits
// for is not rendered), then shrink to a phone and client-side navigate, as providers.spec.ts does.
async function phoneAt(page: Page, view: "user" | "admin", path: string): Promise<void> {
  await gotoConsole(page, view);
  await page.setViewportSize(PHONE);
  await navToRoute(page, path);
}

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "tier-b e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Tier B at 390px", () => {
  // #2846: an unrecorded video row shows no play glyph, and a long title wraps instead of ending in an ellipsis.
  test("Getting started: unrecorded videos have no play icon and no title is cut off", async ({ page }) => {
    await phoneAt(page, "user", "/setup");
    const titles = page.getByTestId("episode-title");
    await expect(titles.first()).toBeVisible();
    const unrecorded = page.locator("div:has(> [data-testid=episode-title])").filter({ hasText: "Not recorded yet" });
    expect(await unrecorded.count()).toBeGreaterThan(0);
    await expect(unrecorded.locator("svg.lucide-play")).toHaveCount(0);
    const cut = await titles.evaluateAll((els) => els.filter((e) => e.scrollWidth > e.clientWidth + 1).length);
    expect(cut).toBe(0);
  });
});

test.describe("Tier B at desktop width", () => {
  // #2848: the five "How it works" cards share one top and one height instead of centring at their own.
  test("Setup intro: the five cards line up", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/setup");
    await page.getByRole("button", { name: "Show intro" }).click();
    const cards = page.getByTestId("how-it-works-card");
    await expect(cards).toHaveCount(5);
    const boxes = await cards.evaluateAll((els) =>
      els.map((e) => {
        const r = e.getBoundingClientRect();
        return { top: Math.round(r.top), height: Math.round(r.height) };
      }),
    );
    expect(new Set(boxes.map((b) => b.top)).size).toBe(1);
    expect(new Set(boxes.map((b) => b.height)).size).toBe(1);
  });
});

test.describe("Tier B runs list", () => {
  // #2905: the member's empty card sits apart from the composer card above it instead of touching it.
  test("Runs (member, no runs): the empty card does not butt against the composer", async ({ page }) => {
    await asRealMember(page);
    await page.route(/\/api\/v1\/runs(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs");
    const card = (l: Locator) => l.locator("xpath=ancestor-or-self::*[contains(@class,'rounded-xl')][1]");
    const composer = card(page.getByRole("form", { name: "Start a run" }));
    const empty = card(page.getByText("Runs you launch appear here"));
    await expect(empty).toBeVisible();
    const [c, e] = [(await composer.boundingBox())!, (await empty.boundingBox())!];
    expect(e.y - (c.y + c.height)).toBeGreaterThanOrEqual(8);
  });
});

test.describe("Tier B run pages", () => {
  // #2908: a run's own page keeps its parent item lit, in both views.
  for (const view of ["user", "admin"] as const) {
    test(`Run detail (${view} view): the sidebar's Runs item stays highlighted`, async ({ page }) => {
      await gotoConsole(page, view);
      const id = await seedRun(page);
      await navToRoute(page, `${view === "admin" ? "/admin" : ""}/runs/${id}`);
      await expect(page.getByRole("tab", { name: /Overview/ })).toBeVisible();
      await expect(sidebarLink(page, "Runs")).toHaveAttribute("aria-current", "page");
    });
  }
});
