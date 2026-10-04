/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * AN OBSERVER'S GRID IS PINNED TO THE WRITER'S (term-t16), AGAINST A REAL TMUX.
 *
 * A second tab of the same person is admitted read-only in a much smaller
 * window. It must render tmux's layout at the writer's columns and rows, not
 * its own container's, and follow the writer when the writer resizes.
 */
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy/run-cockpit";
import { readGrid } from "./terminal-grid";

const settle = async (page: Page) => {
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await page.waitForTimeout(800);
};

test("a read-only tab renders the writer's grid and follows the writer's resize", async ({ page, context }) => {
  test.setTimeout(120_000);
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(context, runId);
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  await expect(page.locator(".xterm-screen").first()).toBeVisible();
  await settle(page);
  const writer = await readGrid(page);

  const observer = await context.newPage();
  // Still much smaller than the writer, and wide enough (md, 768px) for the sidebar gotoConsole waits on.
  await observer.setViewportSize({ width: 800, height: 560 });
  await gotoConsole(observer);
  await navToRoute(observer, `/runs/${runId}`);
  await expect(observer.getByText(RUN_COCKPIT.watchingReadOnly)).toBeVisible({ timeout: 30_000 });
  await settle(observer);
  await expect.poll(() => readGrid(observer), { timeout: 15_000 }).toEqual(writer);

  // The writer's window changes size; the observer follows.
  await page.setViewportSize({ width: 1000, height: 640 });
  await settle(page);
  const resized = await readGrid(page);
  expect(resized).not.toEqual(writer);
  await expect.poll(() => readGrid(observer), { timeout: 15_000 }).toEqual(resized);
});
