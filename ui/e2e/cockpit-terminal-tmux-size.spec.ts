/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE WRITER'S GEOMETRY, AGAINST A REAL TMUX (term-t17, RC6).
 *
 * The shell inside tmux must see exactly the grid xterm.js shows: the size the
 * attach URL carries on load, the size of the last resize frame after a pane
 * change, and the same size again after a reload (a fresh attach must not
 * reflow the window to 80x24). Read from the shell itself with `stty size`.
 */
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { settledGrid } from "./terminal-grid";
import { termText } from "./terminal-text";

type Size = { cols: number; rows: number };

// The geometry the browser last told the daemon: the attach URL's, then each resize frame.
// `onUrl` records the size each attach URL carried (undefined when it carried none).
function trackGeometry(page: Page, onUrl: (s: Size | undefined) => void): { current: () => Size | undefined; reset: () => void } {
  let size: Size | undefined;
  page.on("websocket", (ws) => {
    const u = new URL(ws.url());
    const cols = Number(u.searchParams.get("cols"));
    const rows = Number(u.searchParams.get("rows"));
    size = cols > 0 && rows > 0 ? { cols, rows } : undefined;
    onUrl(size);
    ws.on("framesent", (f) => {
      if (typeof f.payload !== "string") return;
      try {
        const m = JSON.parse(f.payload);
        if (m?.type === "resize") size = { cols: m.cols, rows: m.rows };
      } catch {
        /* not a control frame */
      }
    });
  });
  return { current: () => size, reset: () => (size = undefined) };
}

test("the shell sees the grid xterm shows: on load, after a resize, after a reload", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  const urlSizes: Array<Size | undefined> = [];
  const geo = trackGeometry(page, (s) => urlSizes.push(s));
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = () => page.locator(".xterm-screen").first();
  await expect(screen()).toBeVisible();
  const read = async () => (await termText(screen()).catch(() => "")) as string;
  let n = 0;
  // The shell's own view of its window. Typing is retried: a fresh attach can
  // be admitted read-only for a moment while the previous socket is released.
  const shellSize = async (): Promise<Size> => {
    const tag = `sz${++n}`;
    const re = new RegExp(`${tag}-(\\d+)x(\\d+)`);
    await expect
      .poll(
        async () => {
          if (!re.test(await read())) {
            await screen().click();
            await page.keyboard.type(`echo ${tag}-$(stty size | tr ' ' x)`);
            await page.keyboard.press("Enter");
          }
          await page.waitForTimeout(500);
          return re.test(await read());
        },
        { timeout: 30_000 },
      )
      .toBe(true);
    const m = (await read()).match(re)!;
    return { rows: Number(m[1]), cols: Number(m[2]) };
  };
  const reported = async (): Promise<Size> => {
    await expect.poll(() => geo.current()).toBeDefined();
    return geo.current()!;
  };

  // A new attach carries the real size in its URL, so the PTY never starts at 0x0
  // (tmux would then size the window 80x24 and reflow it on the first resize).
  await expect.poll(() => urlSizes.length).toBeGreaterThan(0);
  expect(urlSizes[0], "the first attach URL carries cols and rows").toBeDefined();

  // On load, once the layout has settled.
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await settledGrid(page, screen());
  expect(await shellSize()).toEqual(await reported());

  // A real change in the pane's height reaches the shell.
  const before = await reported();
  await page.getByTestId("run-terminal-pane").evaluate((el) => el.style.setProperty("height", "300px", "important"));
  await expect.poll(async () => (await reported()).rows).not.toBe(before.rows);
  await settledGrid(page, screen());
  const resized = await shellSize();
  expect(resized).toEqual(await reported());

  // A fresh attach lands on the writer's size, not tmux's 80x24 default.
  geo.reset();
  await page.reload();
  await expect(screen()).toBeVisible();
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await settledGrid(page, screen());
  await expect.poll(() => urlSizes.length).toBeGreaterThan(1);
  expect(urlSizes[urlSizes.length - 1], "the re-attach URL carries cols and rows").toBeDefined();
  const after = await shellSize();
  expect(after).toEqual(await reported());
  expect(after).not.toEqual({ cols: 80, rows: 24 });
});
