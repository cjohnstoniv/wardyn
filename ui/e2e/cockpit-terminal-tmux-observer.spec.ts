/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * AN OBSERVER NEVER CLAMPS THE WRITER, AGAINST A REAL TMUX (term-t7, RC6).
 *
 * run-ui-e2e.sh serves this spec from the e2etmux build: the production attach
 * endpoint, holder registry and pump drive a real tmux. A second tab of the same
 * person is admitted read-only; under tmux's `window-size latest` its tmux
 * client used to resize the shared window to 80x24 the moment it attached, so
 * the writer's shell lost its real size. With the observer attached as an
 * observer (tmux's ignore-size flag) the writer's window must not move, and
 * when the observer is promoted the window follows the PROMOTED client's own
 * size. Read from the shell itself with `stty size`.
 */
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy/run-cockpit";
import { readGrid } from "./terminal-grid";

type Size = { cols: number; rows: number };

// The shell's own view of its window, asked through the tab's terminal. Typing
// is retried: a tab that has just been promoted, or re-attached, can be
// read-only for a moment.
async function shellSize(page: Page, tag: string): Promise<Size> {
  const screen = page.locator(".xterm-screen").first();
  const read = async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")) as string;
  const re = new RegExp(`${tag}-(\\d+)x(\\d+)`);
  await expect
    .poll(
      async () => {
        if (!re.test(await read())) {
          await screen.click();
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
}

const settle = async (page: Page) => {
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await page.waitForTimeout(800);
};

test("a second tab never clamps the writer's window, and a promotion sizes it to the promoted tab", async ({ page, context }) => {
  // Two tabs, a promotion and three shell round trips: past the default 30s.
  test.setTimeout(120_000);
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(context, runId);
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  await expect(page.locator(".xterm-screen").first()).toBeVisible();
  await settle(page);

  // The writer's shell sees the grid xterm shows, which is not 80x24.
  const grid = await readGrid(page);
  const before = await shellSize(page, "wr1");
  expect(before).toEqual(grid);
  expect(before).not.toEqual({ cols: 80, rows: 24 });

  // A second tab, in a small window, watches the same run.
  const observer = await context.newPage();
  await observer.setViewportSize({ width: 760, height: 560 });
  await gotoConsole(observer);
  await navToRoute(observer, `/runs/${runId}`);
  await expect(observer.getByText(RUN_COCKPIT.watchingReadOnly)).toBeVisible({ timeout: 30_000 });
  await settle(observer);

  // The observer's tmux client must not have resized the writer's window.
  expect(await shellSize(page, "wr2")).toEqual(before);

  // The writer's tab goes away; the observer is promoted and its OWN size is
  // the window's size, not the departed writer's.
  await page.close();
  await expect(observer.getByText(RUN_COCKPIT.watchingReadOnly)).toBeHidden({ timeout: 30_000 });
  await settle(observer);
  const promotedGrid = await readGrid(observer);
  expect(promotedGrid).not.toEqual(grid);
  expect(await shellSize(observer, "pr1")).toEqual(promotedGrid);
});
