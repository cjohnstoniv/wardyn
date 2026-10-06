/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * COPY, AGAINST A REAL TMUX (term-t3a).
 *
 * run-ui-e2e.sh serves this spec from the e2etmux build: the production attach
 * endpoint drives a real tmux started with deploy/images/common/tmux.conf, which
 * owns the mouse. A plain drag, double-click or triple-click is a tmux selection
 * that tmux copies as OSC 52 with an EMPTY selector; the browser must turn those
 * exact bytes into an offer, and refuse what the sandbox emits on its own.
 */
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { clickCell, dragCells, rowOf, settledGrid } from "./terminal-grid";
import { termText } from "./terminal-text";

async function open(page: Page) {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  // The browser clipboard outlives a test: start each from empty.
  await page.evaluate(() => navigator.clipboard.writeText(""));
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();
  const read = async () => (await termText(screen).catch(() => "")) as string;
  await screen.click();
  // Typing is retried: a fresh attach can be admitted read-only for a moment.
  await expect
    .poll(
      async () => {
        if (!(await read()).includes("ready-sentinel")) {
          await screen.click();
          await page.keyboard.type("printf 'ready-%s\\n' sentinel");
          await page.keyboard.press("Enter");
        }
        await page.waitForTimeout(500);
        return (await read()).includes("ready-sentinel");
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  return { screen, read };
}
const clipboard = (page: Page) => page.evaluate(() => navigator.clipboard.readText());

test("a dragged line becomes an offer, and Copy writes exactly what was selected", async ({ page }) => {
  const { screen, read } = await open(page);
  await page.keyboard.type("printf 'alpha beta gamma\\n'");
  await page.keyboard.press("Enter");
  await expect.poll(async () => (await read()).split("\n").some((l) => l.trim() === "alpha beta gamma")).toBe(true);
  const grid = await settledGrid(page, screen);
  const row = await rowOf(screen, /^alpha beta gamma\s*$/);

  // Nothing was copied by tmux's own copy path alone.
  expect(await clipboard(page)).toBe("");
  // Released on the blank cell after the text: tmux copies up to the cell before it.
  await dragCells(page, screen, grid, [0, row], [16, row]);
  const offer = page.getByTestId("terminal-copy-offer");
  await expect(offer).toBeVisible();
  await expect(page.getByTestId("terminal-copy-offer-text")).toHaveText("alpha beta gamma");
  expect(await clipboard(page)).toBe("");
  await offer.getByRole("button", { name: "Copy" }).click();
  expect(await clipboard(page)).toBe("alpha beta gamma");
  await expect(offer).toHaveCount(0);
});

test("a double-click copies the word and a triple-click the line", async ({ page }) => {
  const { screen, read } = await open(page);
  await page.keyboard.type("printf 'alpha beta gamma\\n'");
  await page.keyboard.press("Enter");
  await expect.poll(async () => (await read()).split("\n").some((l) => l.trim() === "alpha beta gamma")).toBe(true);
  const grid = await settledGrid(page, screen);
  const row = await rowOf(screen, /^alpha beta gamma\s*$/);
  const offer = page.getByTestId("terminal-copy-offer");
  const text = page.getByTestId("terminal-copy-offer-text");

  await clickCell(page, screen, grid, [7, row], 2); // inside "beta"
  await expect(text).toHaveText("beta");
  await offer.getByRole("button", { name: "Dismiss" }).click();
  await expect(offer).toHaveCount(0);

  await clickCell(page, screen, grid, [7, row], 3);
  await expect(text).toHaveText("alpha beta gamma");
  await offer.getByRole("button", { name: "Copy" }).click();
  expect(await clipboard(page)).toBe("alpha beta gamma");
});

test("tmux set-buffer -w from the shell, with no gesture, makes no offer", async ({ page }) => {
  const { read } = await open(page);
  await page.keyboard.type("tmux set-buffer -w hostile-payload; printf 'after-%s\\n' hostile");
  await page.keyboard.press("Enter");
  await expect.poll(async () => (await read()).split("\n").some((l) => l.trim() === "after-hostile")).toBe(true);
  await page.waitForTimeout(500);
  await expect(page.getByTestId("terminal-copy-offer")).toHaveCount(0);
  expect(await clipboard(page)).toBe("");
});
