/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Terminal fit geometry: FitAddon measures the parent of `.xterm`, so a padded
 * mount over-counts rows and clips the last one at some panel heights. This
 * sweeps the pane height and requires `.xterm-screen` to stay inside the
 * padded wrapper's content box at every step; a failure lists the heights.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { settledGrid } from "./terminal-grid";

const HEIGHTS = Array.from({ length: 33 }, (_, i) => 300 + i * 7); // 300..524

test("the terminal grid stays inside its padded wrapper at every panel height", async ({ page }) => {
  // 33 heights, each held until the grid has been still for 600 ms.
  test.setTimeout(90_000);
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, () => {});

  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const pane = page.getByTestId("run-terminal-pane");
  const screen = pane.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();

  const failing: string[] = [];
  for (const h of HEIGHTS) {
    await pane.evaluate((el, height) => el.style.setProperty("height", `${height}px`, "important"), h);
    // Let the ResizeObserver-driven fit settle.
    await settledGrid(page, screen);
    const m = await pane.evaluate((el) => {
      const wrap = el.querySelector<HTMLElement>('[data-testid="run-terminal-wrapper"]');
      const scr = el.querySelector<HTMLElement>(".xterm-screen");
      if (!wrap || !scr) return null;
      const cs = getComputedStyle(wrap);
      const bottom = wrap.getBoundingClientRect().bottom - parseFloat(cs.paddingBottom) - parseFloat(cs.borderBottomWidth);
      return { screenBottom: scr.getBoundingClientRect().bottom, bottom };
    });
    if (!m || m.screenBottom > m.bottom + 0.5) failing.push(`${h}`);
  }
  expect(failing, `failing heights: ${failing.join(", ")}`).toEqual([]);
});
