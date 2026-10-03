/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Terminal size protocol: the attach URL carries the real geometry, the socket
 * gets exactly one resize frame per real change, and a same-size refit sends
 * none.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";

test("one resize frame per real size change, none for a same-size refit", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  const urls: string[] = [];
  const frames: Array<{ cols: number; rows: number }> = [];
  await stubAttachSocket(page, (_n, ws) => {
    urls.push(ws.url());
    ws.onMessage((m) => {
      if (typeof m !== "string") return;
      try {
        const f = JSON.parse(m);
        if (f?.type === "resize") frames.push({ cols: f.cols, rows: f.rows });
      } catch {
        /* not a control frame */
      }
    });
  });

  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  const pane = page.getByTestId("run-terminal-pane");
  await expect(pane.locator(".xterm-screen").first()).toBeVisible();
  await expect.poll(() => frames.length).toBeGreaterThan(0);

  const u = new URL(urls[0]);
  expect(Number(u.searchParams.get("cols"))).toBeGreaterThan(0);
  expect(Number(u.searchParams.get("rows"))).toBeGreaterThan(0);
  // The page's layout animates in, so the first frames are real changes. Wait
  // for them to stop, then none may repeat its predecessor.
  let prev = -1;
  await expect
    .poll(async () => {
      const n = frames.length;
      const stable = n === prev;
      prev = n;
      await page.waitForTimeout(400);
      return stable;
    })
    .toBe(true);
  const settled = frames.length;
  frames.forEach((f, i) => {
    if (i > 0) expect(f).not.toEqual(frames[i - 1]);
  });

  // Same-size refit: a window resize event with no geometry change.
  await page.evaluate(() => window.dispatchEvent(new Event("resize")));
  await page.waitForTimeout(500);
  expect(frames.length).toBe(settled);

  // A real change: exactly one more frame (transitions off so the box jumps).
  await page.addStyleTag({ content: "*{transition:none!important;animation:none!important}" });
  await pane.evaluate((el) => el.style.setProperty("height", "300px", "important"));
  await expect.poll(() => frames.length).toBe(settled + 1);
  await page.waitForTimeout(500);
  expect(frames.length).toBe(settled + 1);
});
