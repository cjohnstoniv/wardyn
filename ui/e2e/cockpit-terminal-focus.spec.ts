/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Run cockpit terminal focus (term-t5). Asserts where focus REALLY is
 * (`document.activeElement`), never mocked focus() call counts: the earlier
 * fix pinned call counts and passed while the bug remained. Same hermetic
 * attach stubs as cockpit-terminal.spec.ts.
 */
import type { Locator, Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import {
  attachModeFrame,
  findRunningFixture,
  stubAttachSocket,
  stubAttachTicket,
  stubInteractiveRun,
} from "./attach-stub";

// The buttons are clicked with force: Playwright's actionability "stable"
// check never settles on this page, which is unrelated to what is under test.
test.describe.configure({ mode: "serial" });

const focusInXterm = (page: Page) =>
  page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea") ?? false);

async function openTerminal(page: Page) {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, (_n, ws) => {
    ws.send(attachModeFrame(false));
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  const pane = page.getByTestId("run-terminal-pane");
  await expect(pane.locator(".xterm-screen").first()).toBeVisible();
  // A writable terminal takes focus on load; wait for that, then move focus
  // away so each case starts from a terminal that is not focused.
  await expect.poll(() => focusInXterm(page)).toBe(true);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  expect(await focusInXterm(page)).toBe(false);
  return pane;
}

// Viewport coordinates of the wrapper, scrolled fully into view so a raw
// mouse click lands on it rather than outside the viewport.
async function wrapperBox(pane: Locator) {
  const wrapper = pane.getByTestId("run-terminal-wrapper");
  await wrapper.evaluate((el) => el.scrollIntoView({ block: "nearest" }));
  return wrapper.boundingBox();
}

test.describe("Run cockpit terminal focus", () => {
  test("a click on the wrapper padding focuses the terminal", async ({ page }) => {
    const pane = await openTerminal(page);
    const box = await wrapperBox(pane);
    expect(box).not.toBeNull();
    await page.mouse.click(box!.x + 2, box!.y + 2);
    await expect.poll(() => focusInXterm(page)).toBe(true);
  });

  test("a click in the strip below the last row focuses the terminal", async ({ page }) => {
    const pane = await openTerminal(page);
    const box = await wrapperBox(pane);
    expect(box).not.toBeNull();
    await page.mouse.click(box!.x + box!.width / 2, box!.y + box!.height - 2);
    await expect.poll(() => focusInXterm(page)).toBe(true);
  });

  test("focus is in the terminal after Fullscreen", async ({ page }) => {
    // A headless browser may never grant native fullscreen, so drive the
    // in-page fallback path; it settles through the same onSettled refocus.
    await page.addInitScript(() => {
      (Element.prototype as { requestFullscreen?: unknown }).requestFullscreen = undefined;
    });
    const pane = await openTerminal(page);
    await pane.getByRole("button", { name: "Fullscreen" }).click({ force: true });
    await expect(pane.getByRole("button", { name: "Exit fullscreen" })).toBeVisible();
    await expect.poll(() => focusInXterm(page)).toBe(true);
  });

  test("focus is in the terminal after Redraw", async ({ page }) => {
    const pane = await openTerminal(page);
    await pane.getByRole("button", { name: "Redraw terminal" }).click({ force: true });
    await expect.poll(() => focusInXterm(page)).toBe(true);
  });
});
