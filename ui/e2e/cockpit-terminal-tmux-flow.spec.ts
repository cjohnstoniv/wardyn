/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * OUTPUT FLOW CONTROL, AGAINST A REAL TMUX (term-t14).
 *
 * 50 MB of output must not freeze the page: the page stays responsive while it
 * streams, the final screen is the tail of the output, and input still echoes
 * within the budget afterwards. When the stream backs xterm up past the flow
 * watermark the console must send `pause` and then `resume`; tmux coalesces
 * the stream, so whether the backlog gets that deep depends on the machine and
 * the spec measures it rather than assuming it. The client unit test and the
 * server pause/resume tests pin the pause/resume mechanism itself.
 *
 * Named cockpit-terminal-tmux-* so run-ui-e2e.sh serves it from the e2etmux build.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { termText } from "./terminal-text";
import { FLOW_HIGH_WATERMARK } from "../src/app/components/attach-terminal-flow";

const MAIN_THREAD_BUDGET_MS = 200;
const ECHO_BUDGET_MS = 200;
const ECHO_POLL_MS = 20;

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

  // Record the deepest write backlog xterm reaches (bytes written, not yet parsed:
  // the quantity the flow control watches), so the pause assertion below only
  // applies when the stream really crossed the watermark.
  await page.evaluate(() => {
    const w = window as unknown as {
      __wardynTerm: { terms: Set<{ write(d: Uint8Array, cb?: () => void): void }> };
      __flowPeak: number;
    };
    const term = [...w.__wardynTerm.terms][0]!;
    const write = term.write.bind(term);
    let pending = 0;
    w.__flowPeak = 0;
    term.write = (data, cb) => {
      pending += data.length;
      w.__flowPeak = Math.max(w.__flowPeak, pending);
      write(data, () => {
        pending -= data.length;
        cb?.();
      });
    };
  });

  // A slower renderer than the sandbox, so the write backlog is as likely as it can be to build.
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

  // A backlog past the watermark must have been paused, and every pause ended resumed.
  const peak = await page.evaluate(() => (window as unknown as { __flowPeak: number }).__flowPeak);
  if (peak > FLOW_HIGH_WATERMARK) {
    expect(frames.pause, `backlog peaked at ${peak} bytes, past the ${FLOW_HIGH_WATERMARK} watermark`).toBeGreaterThan(0);
  }
  await expect.poll(() => frames.resume).toBe(frames.pause);

  // And input still echoes promptly once the stream is over. The command's output
  // (flow-echo-42) differs from what the typed line shows, so only the shell's
  // answer satisfies the poll. The poll's own resolution adds up to ECHO_POLL_MS.
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });
  await page.keyboard.type("echo flow-echo-$((6*7))");
  const t0 = Date.now();
  await page.keyboard.press("Enter");
  let echoMs = Number.POSITIVE_INFINITY;
  while (Date.now() - t0 < 5_000) {
    if ((await read()).includes("flow-echo-42")) {
      echoMs = Date.now() - t0;
      break;
    }
    await page.waitForTimeout(ECHO_POLL_MS);
  }
  expect(echoMs, "input-to-echo ms").toBeLessThan(ECHO_BUDGET_MS);
});
