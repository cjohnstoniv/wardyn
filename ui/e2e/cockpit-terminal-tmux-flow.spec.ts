/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * OUTPUT FLOW CONTROL, AGAINST A REAL TMUX (term-t14).
 *
 * 50 MB of output must not freeze the page: xterm's write backlog crosses its
 * watermark, the console sends `pause` and `resume` control frames, and the
 * server stops reading the exec while paused. The page stays responsive while
 * it streams and the final screen is the tail of the output.
 *
 * Named cockpit-terminal-tmux-* so run-ui-e2e.sh serves it from the e2etmux build.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { termText } from "./terminal-text";

const MAIN_THREAD_BUDGET_MS = 200;

test("50 MB of output keeps the page responsive and ends on the right screen", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  const frames = { pause: 0, resume: 0 };
  page.on("websocket", (ws) => {
    ws.on("framesent", (f) => {
      if (typeof f.payload !== "string") return;
      try {
        const m = JSON.parse(f.payload);
        if (m?.type === "pause") frames.pause++;
        if (m?.type === "resume") frames.resume++;
      } catch {
        /* not a control frame */
      }
    });
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();
  const read = async () => (await termText(screen).catch(() => "")) as string;

  // A slower renderer than the sandbox, so the write backlog crosses the watermark.
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 });

  await screen.click();
  await page.keyboard.type("echo flow-ready");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 20_000 }).toContain("flow-ready");

  // 50 MiB of 100-byte lines, then a sentinel the final screen must show.
  await page.keyboard.type(`yes "$(printf 'x%.0s' $(seq 1 99))" | head -c 52428800; echo; echo flow-done-$((6*7))`);
  await page.keyboard.press("Enter");

  // While it streams, the main thread answers within the budget (a frozen
  // page misses it by seconds). Probes stop once the sentinel is on screen.
  const worst: number[] = [];
  const deadline = Date.now() + 120_000;
  while (Date.now() < deadline) {
    const ms = await page.evaluate(
      () => new Promise<number>((resolve) => {
        const t0 = performance.now();
        setTimeout(() => resolve(performance.now() - t0), 0);
      }),
    );
    worst.push(ms);
    if ((await read()).includes("flow-done-42")) break;
  }
  expect(await read()).toContain("flow-done-42");
  worst.sort((a, b) => a - b);
  const p95 = worst[Math.floor(worst.length * 0.95)] ?? 0;
  expect(p95, `main-thread p95 over ${worst.length} probes`).toBeLessThan(MAIN_THREAD_BUDGET_MS);

  // Flow control actually engaged, and ended resumed.
  expect(frames.pause).toBeGreaterThan(0);
  await expect.poll(() => frames.resume).toBe(frames.pause);

  // And input still echoes promptly once the stream is over.
  await page.keyboard.type("echo flow-echo");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 5_000 }).toContain("flow-echo");
});
