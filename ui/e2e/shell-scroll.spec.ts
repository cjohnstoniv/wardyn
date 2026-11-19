/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, chooseFiles, continueNewRun, gotoConsole, mockPreflightClear, navTo, openNewRun, walkGuidedToFiles } from "./fixtures";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

// The shell scrolls <main> and nothing else. An absolutely positioned sr-only element (a CopyButton or RunRef
// live region, New run's status region) used to take the viewport as its containing block, escape <main> and
// make the document taller than the h-screen shell; a wheel over the sidebar, which has nothing to scroll,
// then slid the whole shell up and left a blank band under <main>.
//
// Audit is the long page the seeded backend fills (about 19 events, one RunRef each); the New run Tools step
// is the other, whose live region is the last thing in the page.

const documentOverflow = (page: Page) =>
  page.evaluate(() => (document.scrollingElement?.scrollHeight ?? 0) - window.innerHeight);
const mainTop = (page: Page) => page.evaluate(() => document.querySelector("main")!.getBoundingClientRect().top);
const mainScrolls = (page: Page) =>
  page.evaluate(() => {
    const m = document.querySelector("main")!;
    return m.scrollHeight > m.clientHeight;
  });

/** Two frames, so a scroll the wheel started has been applied before a negative is read. */
const settle = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))));

async function expectShellHolds(page: Page) {
  expect(await documentOverflow(page)).toBeLessThanOrEqual(1);
  const top = await mainTop(page);
  const nav = (await page.locator("aside").first().boundingBox())!;
  await page.mouse.move(nav.x + nav.width / 2, nav.y + nav.height / 2);
  await page.mouse.wheel(0, 800);
  await settle(page);
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  expect(await mainTop(page)).toBe(top);
}

test.describe("shell scroll containment", () => {
  test("Audit at 1280x720: the document is no taller than the window and a wheel over the sidebar does not move the shell", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    await gotoConsole(page, "admin");
    await navTo(page, "Audit");
    await expect(page.getByText(/^\d+ events?$/)).toBeVisible();
    // The page has to be long for this to prove anything.
    expect(await mainScrolls(page)).toBe(true);
    await expectShellHolds(page);
  });

  for (const height of [720, 480]) {
    test(`New run Tools step at 1280x${height}: the document is no taller than the window and a wheel over the sidebar does not move the shell`, async ({ page }) => {
      await mockPreflightClear(page);
      await gotoConsole(page);
      await openNewRun(page);
      await walkGuidedToFiles(page);
      await chooseFiles(page);
      await continueNewRun(page, NR_TOOLS.TITLE);
      // The steps are walked at the default 720; the viewport shrinks only once the Tools step is up, and the layout settles
      // before anything is read. At 480 the step is certainly longer than <main>; at 720 it may or may not be, and holds either way.
      await page.setViewportSize({ width: 1280, height });
      await settle(page);
      if (height === 480) expect(await mainScrolls(page)).toBe(true);
      await expectShellHolds(page);
    });
  }
});
