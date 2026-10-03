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
 */

import { expect, test } from "@playwright/test";
import { MEMBER_EMAIL, SANDBOX_UP, dexSignIn } from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1");

test("the wheel scrolls tmux history and does not recall bash history", async ({ page }) => {
  await dexSignIn(page, MEMBER_EMAIL);
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("terminal wheel walk");
  await page.getByRole("radio", { name: /^Terminal/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  const read = async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")) as string;

  await screen.click();
  // A history entry for the wheel to wrongly recall, then enough output to fill
  // more than one screen so there is scrollback to reach.
  await page.keyboard.type("echo recall-sentinel");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: SANDBOX_UP }).toContain("recall-sentinel");
  await page.keyboard.type("seq 1 400");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 30_000 }).toContain("400");

  // Wheel up over the terminal: tmux enters copy-mode and shows its position
  // indicator, and the first lines of the output come into view.
  const box = (await screen.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let i = 0; i < 20; i++) await page.mouse.wheel(0, -300);
  await expect.poll(read, { timeout: 15_000 }).toMatch(/\[\d+\/\d+\]/);

  // Leave copy-mode; the prompt line is empty, so no history was recalled.
  await page.keyboard.press("q");
  await expect.poll(read, { timeout: 15_000 }).toContain("400");
  const lines = (await read()).split("\n").map((l) => l.trimEnd()).filter(Boolean);
  expect(lines[lines.length - 1]).toMatch(/\$$/);
});
