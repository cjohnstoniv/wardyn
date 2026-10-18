/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B, run and setup screens: the pins that need a real layout engine (widths,
// wrapping, computed positions). Text-shaped fixes are pinned in vitest next to their screen.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";

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
