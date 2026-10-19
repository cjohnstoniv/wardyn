/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE BROWSER TERMINAL'S WHEEL, AGAINST A REAL SANDBOX (term-t1).
 *
 * tmux owns the mouse wheel in every bundled image (deploy/images/common/
 * tmux.conf). In a shell pane the wheel must scroll tmux history; before this
 * change tmux sat on xterm's alternate buffer with mouse off, so xterm.js
 * turned each wheel notch into Up/Down and the shell recalled bash history.
 *
 * Red on an image built from origin/main's tmux.conf, green on a lane image.
 * Needs a live cluster whose sandbox image carries the lane's tmux.conf.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as every other file here.
 * Signs in through Dex, or from a stored session (WARDYN_WALK_STORAGE_STATE) on an estate without Dex.
 */

import { expect, test } from "@playwright/test";
import { ENTRA_STORAGE_STATE, MEMBER_EMAIL, SANDBOX_UP, launchFromReview, viewLaunchedRun, walkNewRunToReview, walkSignIn } from "./helpers";
import { termText, waitForShell } from "../terminal-text";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1");
test.use({ storageState: ENTRA_STORAGE_STATE });

test("the wheel scrolls tmux history and does not recall bash history", async ({ page }) => {
  await walkSignIn(page, MEMBER_EMAIL);
  await page.goto("/runs/new");
  // An Environment sandbox is the terminal: interactive, no agent.
  await walkNewRunToReview(page, { shape: "environment" });
  await launchFromReview(page, "terminal wheel walk");
  await viewLaunchedRun(page);

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  const read = async () => (await termText(screen).catch(() => "")) as string;

  await screen.click();
  await waitForShell(page, screen);
  // A history entry for the wheel to wrongly recall, then enough output to fill
  // more than one screen so there is scrollback to reach.
  await page.keyboard.type("echo recall-sentinel");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: SANDBOX_UP }).toContain("recall-sentinel");
  await page.keyboard.type("seq 1 400");
  await page.keyboard.press("Enter");
  // A line of its own: the typed `seq 1 400` also contains "400".
  await expect.poll(read, { timeout: 30_000 }).toMatch(/^400$/m);

  // Wheel up over the terminal: tmux enters copy-mode and shows its position
  // indicator, and the first lines of the output come into view.
  const box = (await screen.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let i = 0; i < 20; i++) await page.mouse.wheel(0, -300);
  // Scrolled up: a position past 0, not the indicator's initial [0/N].
  await expect.poll(read, { timeout: 15_000 }).toMatch(/\[[1-9]\d*\/\d+\]/);

  // Leave copy-mode; the prompt line is empty, so no history was recalled.
  await page.keyboard.press("q");
  await expect.poll(read, { timeout: 15_000 }).toMatch(/^400$/m);
  // The prompt returns after the last number is drawn; poll for it, not for one read that can land between.
  await expect
    .poll(
      async () => {
        const lines = (await read()).split("\n").map((l) => l.trimEnd()).filter(Boolean);
        return lines[lines.length - 1] ?? "";
      },
      { timeout: 15_000 },
    )
    .toMatch(/\$$/);
});
