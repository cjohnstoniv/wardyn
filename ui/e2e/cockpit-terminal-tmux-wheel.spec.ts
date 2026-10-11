/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE BROWSER TERMINAL'S WHEEL, AGAINST A REAL TMUX (term-t17, S1).
 *
 * run-ui-e2e.sh serves this spec from the e2etmux build: the production attach
 * endpoint drives a real tmux started with deploy/images/common/tmux.conf. In a
 * shell pane the wheel must scroll tmux history; with mouse off xterm.js turned
 * each notch into Up/Down and the shell recalled bash history instead.
 */
import { test, expect, gotoConsole, navToRoute, ADMIN_TOKEN } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { termText } from "./terminal-text";

test("the wheel scrolls tmux history and does not recall bash history", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();
  const read = async () => (await termText(screen).catch(() => "")) as string;

  await screen.click();
  // A fresh attach can be admitted read-only for a moment, and keys typed then
  // are dropped: retry a readiness line until the shell echoes it (as the copy
  // spec does), and only then type the lines the test depends on.
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
  // A history entry for the wheel to wrongly recall, then more output than one
  // screen so there is scrollback to reach.
  await page.keyboard.type("echo recall-sentinel");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 20_000 }).toContain("recall-sentinel");
  await page.keyboard.type("seq 1 400");
  await page.keyboard.press("Enter");
  await expect.poll(read).toContain("400");

  // Wheel up over the terminal: tmux enters copy-mode and shows its position.
  const box = (await screen.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let i = 0; i < 20; i++) await page.mouse.wheel(0, -300);
  await expect.poll(read).toMatch(/\[\d+\/\d+\]/);

  // Leave copy-mode: the prompt line is empty, so nothing was recalled.
  await page.keyboard.press("q");
  await expect.poll(read).toContain("400");
  // The prompt returns after the last number is drawn; wait for it, not for a
  // single read that can land between the two.
  await expect
    .poll(async () => {
      const lines = (await read()).split("\n").map((l) => l.trimEnd()).filter(Boolean);
      return lines[lines.length - 1] ?? "";
    })
    .toMatch(/\$$/);
});

test("an attach from a foreign Origin is refused before the upgrade", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  const res = await page.request.get(`/api/v1/runs/${runId}/attach`, {
    headers: {
      Authorization: `Bearer ${ADMIN_TOKEN}`,
      Origin: "http://evil.example",
      Connection: "Upgrade",
      Upgrade: "websocket",
      "Sec-WebSocket-Version": "13",
      "Sec-WebSocket-Key": Buffer.alloc(16, 1).toString("base64"),
    },
  });
  expect(res.status()).toBe(403);
});
