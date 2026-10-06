/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * One terminal per page load and across focus mode (term-t11). /me is held for
 * 1.5s so the terminal mounts on the fail-open operator default and /me then
 * lands: that must not rebuild the terminal (one ticket, one socket). Focus
 * mode moves the terminal host element, so it must not reconnect either.
 * Same hermetic attach stubs as cockpit-terminal.spec.ts.
 */
import { test, expect, consoleAPI } from "./fixtures";
import {
  attachModeFrame,
  findRunningFixture,
  stubAttachSocket,
  stubInteractiveRun,
} from "./attach-stub";
import { settledGrid } from "./terminal-grid";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";

test("a cold load opens one ticket and one socket, focus mode opens none, leaving closes it", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  let tickets = 0;
  await page.route(`**/api/v1/runs/${runId}/attach/ticket`, async (route) => {
    tickets += 1;
    await route.fulfill({ json: { ticket: "e2e-cockpit-ticket" } });
  });
  let closes = 0;
  const sockets = await stubAttachSocket(page, (_n, ws) => {
    ws.send(attachModeFrame(false));
    ws.onClose(() => {
      closes += 1;
    });
  });
  await page.route("**/api/v1/me", async (route) => {
    await new Promise((r) => setTimeout(r, 1500));
    await route.continue();
  });

  await page.goto(`/runs/${runId}`);
  const pane = page.getByTestId("run-terminal-pane");
  await expect(pane.locator(".xterm-screen").first()).toBeVisible({ timeout: 15_000 });
  // Let /me land and any rebuild it would cause show up.
  await page.waitForTimeout(2500);
  expect(tickets).toBe(1);
  expect(sockets.opens()).toBe(1);

  await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click({ force: true });
  await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
  await expect(page.locator(".xterm-screen")).toHaveCount(1);
  // Focus mode has no run-terminal-pane tile, so there is no grid header for settledGrid to read.
  await page.waitForTimeout(500);
  await page.getByRole("button", { name: RUN_COCKPIT.exitFocus }).dispatchEvent("click");
  await expect(pane.locator(".xterm-screen").first()).toBeVisible();
  await settledGrid(page, pane.locator(".xterm-screen").first());
  expect(tickets).toBe(1);
  expect(sockets.opens()).toBe(1);
  expect(closes).toBe(0);

  // Leaving the run screen tears the socket down.
  await page.evaluate(() => {
    window.history.pushState({}, "", "/runs");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect.poll(() => closes).toBe(1);
  const holder = await consoleAPI(page, "GET", `/api/v1/runs/${runId}/attach/holder`);
  expect(JSON.parse(holder.text).held).toBe(false);
});
