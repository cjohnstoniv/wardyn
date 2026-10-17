/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Terminal cell size vs. a late web font: JetBrains Mono is font-display: swap,
 * so xterm can measure its cells from the fallback font. With the woff2 held
 * back 1.5s, once the font arrives the grid's cell size must match what the
 * browser measures for JetBrains Mono (the fallback is 2px shorter per row).
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { readGrid } from "./terminal-grid";

test("the terminal re-measures its cells once a late web font arrives", async ({ page }) => {
  await page.route("**/*.woff2", async (route) => {
    await new Promise((r) => setTimeout(r, 1500));
    await route.continue();
  });
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, () => {});

  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = page.getByTestId("run-terminal-pane").locator(".xterm-screen").first();
  await expect(screen).toBeVisible();

  // The browser's own measure of one cell, once the real font is in.
  await page.evaluate(() => document.fonts.load("13px 'JetBrains Mono'"));
  const glyph = await page.evaluate(() => {
    const ctx = document.createElement("canvas").getContext("2d")!;
    ctx.font = "13px 'JetBrains Mono'";
    const m = ctx.measureText("W");
    return { w: m.width, h: m.fontBoundingBoxAscent + m.fontBoundingBoxDescent };
  });

  await expect
    .poll(
      async () => {
        const grid = await readGrid(page);
        const box = (await screen.boundingBox())!;
        return Math.max(Math.abs(box.width / grid.cols - glyph.w), Math.abs(box.height / grid.rows - glyph.h));
      },
      { message: "cell size should match JetBrains Mono", timeout: 10_000 },
    )
    .toBeLessThan(1);
});
