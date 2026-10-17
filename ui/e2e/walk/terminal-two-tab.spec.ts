/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE TWO-TAB TERMINAL CHECK, AGAINST A REAL SANDBOX (T7).
 *
 * A second tab of the same person on a running terminal is admitted read-only. Under tmux's
 * `window-size latest` its client used to resize the shared window to its own small size the moment it
 * attached, so the first tab's shell lost its real size. The observer now attaches with tmux's ignore-size
 * flag: the writer's window must not move, and when the writer's tab goes away the observer is promoted and
 * the window follows the PROMOTED tab's own size. Read from the shell itself with `stty size`.
 *
 * The hermetic twin (cockpit-terminal-tmux-observer.spec.ts) drives a real tmux with a stubbed run; this one
 * drives a real sandbox on the cluster, so the tab, the attach endpoint, the runner session and tmux are all
 * the shipped ones. Red on an image built before the ignore-size flag, green on a current one.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as every other file here.
 * Signs in through Dex, or from a stored session (WARDYN_WALK_STORAGE_STATE) on an estate without Dex.
 */

import { expect, test, type Page } from "@playwright/test";
import { ENTRA_STORAGE_STATE, MEMBER_EMAIL, SANDBOX_UP, launchFromReview, viewLaunchedRun, walkNewRunToReview, walkSignIn } from "./helpers";
import { RUN_COCKPIT } from "../../src/app/components/wardyn/copy/run-cockpit";
import { readGrid, settledGrid } from "../terminal-grid";
import { termText } from "../terminal-text";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1");
test.use({ storageState: ENTRA_STORAGE_STATE });

type Size = { cols: number; rows: number };

// The shell's own view of its window, asked through the tab's terminal. Typing is retried: a tab that has just
// been promoted, or re-attached, can be read-only for a moment.
async function shellSize(page: Page, tag: string): Promise<Size> {
  const screen = page.locator(".xterm-screen").first();
  const read = async () => (await termText(screen).catch(() => "")) as string;
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
      { timeout: 60_000 },
    )
    .toBe(true);
  const m = (await read()).match(re)!;
  return { rows: Number(m[1]), cols: Number(m[2]) };
}

const settle = async (page: Page) => {
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await settledGrid(page, page.locator(".xterm-screen").first());
};

test("a second tab never clamps the writer's window, and a promotion sizes it to the promoted tab", async ({ page, context }) => {
  test.setTimeout(10 * 60_000);
  await walkSignIn(page, MEMBER_EMAIL);
  await page.goto("/runs/new");
  // An Environment sandbox is the terminal: interactive, no agent.
  await walkNewRunToReview(page, { shape: "environment" });
  await launchFromReview(page, "two tab walk");
  await viewLaunchedRun(page);
  const runUrl = page.url();
  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  await settle(page);

  // The writer's shell sees the grid xterm shows, which is not 80x24.
  const grid = await readGrid(page);
  const before = await shellSize(page, "wr1");
  expect(before).toEqual(grid);
  expect(before).not.toEqual({ cols: 80, rows: 24 });

  // A second tab of the same person, in a much smaller window, watches the same run.
  const observer = await context.newPage();
  await observer.setViewportSize({ width: 800, height: 560 });
  // Same context, so the same session and the renderer pin walkSignIn stored: no second sign-in.
  await observer.goto(runUrl);
  await expect(observer.getByText(RUN_COCKPIT.watchingReadOnly)).toBeVisible({ timeout: 60_000 });
  await expect(observer.locator(".xterm-screen").first()).toBeVisible();
  await settle(observer);

  // The observer's tmux client must not have resized the writer's window.
  expect(await shellSize(page, "wr2")).toEqual(before);

  // The writer's tab goes away; the observer is promoted and its OWN size is the window's size.
  await page.close();
  await expect(observer.getByText(RUN_COCKPIT.watchingReadOnly)).toBeHidden({ timeout: 60_000 });
  await settle(observer);
  const promotedGrid = await readGrid(observer);
  expect(promotedGrid).not.toEqual(grid);
  expect(await shellSize(observer, "pr1")).toEqual(promotedGrid);
});
