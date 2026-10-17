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
import type { Page, Route } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { findRunningFixture, stubInteractiveRun } from "./attach-stub";
import { termText, waitForShell } from "./terminal-text";
import { FLOW_HIGH_WATERMARK } from "../src/app/components/attach-terminal-flow";

const MAIN_THREAD_BUDGET_MS = 200;
// The echo's allowance beyond the idle echo measured first, so a starved runner's own cost is not charged to the page.
const ECHO_BUDGET_MS = 200;

// Types `echo <marker>-42`, presses Enter and returns the ms from that Enter to the frame after xterm
// parsed the answer, timed in the page so the harness's own round trips are not counted.
async function timeEcho(page: Page, marker: string): Promise<number> {
  await page.keyboard.type(`echo ${marker}-$((6*7))`);
  await page.evaluate((m) => {
    type T = {
      rows: number;
      buffer: { active: { viewportY: number; getLine(y: number): { translateToString(trim: boolean): string } | undefined } };
      onWriteParsed(cb: () => void): { dispose(): void };
    };
    const w = window as unknown as { __wardynTerm: { terms: Set<T> }; __echoMs?: number };
    const term = [...w.__wardynTerm.terms][0]!;
    w.__echoMs = undefined;
    let t0 = 0;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Enter" && !t0) t0 = performance.now();
    };
    document.addEventListener("keydown", onKey, { capture: true });
    const parsed = term.onWriteParsed(() => {
      const b = term.buffer.active;
      for (let y = b.viewportY; t0 && y < b.viewportY + term.rows; y++) {
        if (b.getLine(y)?.translateToString(true).includes(`${m}-42`)) {
          parsed.dispose();
          document.removeEventListener("keydown", onKey, { capture: true });
          requestAnimationFrame(() => (w.__echoMs = performance.now() - t0));
          return;
        }
      }
    });
  }, marker);
  await page.keyboard.press("Enter");
  const echo = await page.waitForFunction(() => (window as unknown as { __echoMs?: number }).__echoMs, null, { timeout: 5_000 });
  return (await echo.jsonValue()) as number;
}

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
  await waitForShell(page, screen);

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

  const cdp = await page.context().newCDPSession(page);
  await screen.click();
  await page.keyboard.type("echo flow-ready");
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 20_000 }).toContain("flow-ready");

  // The baseline: the same echo before the throttle, so the budget can allow for the runner's own cost.
  const baselineMs = await timeEcho(page, "flow-base");

  // A slower renderer than the sandbox, so the write backlog is as likely as it can be to build.
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 });

  // While it streams, no page script holds the main thread past the budget, and
  // neither do style and layout. Long-animation-frame entries report every one,
  // so nothing is sampled. Every page script counts except React's scheduler
  // (MessagePort.onmessage), whose renders follow the page's polls, not the
  // output. A timer round trip measured the runner instead: the throttle spins
  // the renderer, and a starved GPU process stalls each commit.
  await page.evaluate(() => {
    type Frame = PerformanceEntry & {
      styleAndLayoutStart: number;
      scripts: Array<{ sourceURL: string; invoker: string; duration: number }>;
    };
    let worst = { what: "nothing over 50 ms", ms: 0 };
    const note = (what: string, ms: number) => {
      if (ms > worst.ms) worst = { what, ms };
    };
    // Only React's scheduler is excluded, so a new output path cannot pass on no data.
    const notStream = ["MessagePort.onmessage"];
    const scan = (frames: PerformanceEntryList) => {
      for (const f of frames as Frame[]) {
        // The page's scripts: the harness's own evaluations have no source.
        for (const s of f.scripts) if (s.sourceURL && !notStream.includes(s.invoker)) note(s.invoker, s.duration);
        if (f.styleAndLayoutStart) note("style and layout", f.startTime + f.duration - f.styleAndLayoutStart);
      }
    };
    // A browser without the entry type would pass on no data, so it fails here.
    if (!PerformanceObserver.supportedEntryTypes.includes("long-animation-frame")) {
      throw new Error("this browser reports no long-animation-frame entries");
    }
    const obs = new PerformanceObserver((list) => scan(list.getEntries()));
    obs.observe({ type: "long-animation-frame" });
    (window as unknown as { __flowWorst: () => typeof worst }).__flowWorst = () => {
      scan(obs.takeRecords());
      return worst;
    };
  });

  // Hold the run-detail poll for the measured window: usePoll waits on its in-flight request, so a
  // held GET pauses it. Released (and unrouted) once the worst frame is read.
  let releasePoll!: () => void;
  const pollHeld = new Promise<void>((r) => (releasePoll = r));
  const hold = async (route: Route) => {
    if (route.request().method() === "GET") await pollHeld;
    return route.fallback();
  };
  await page.route(`**/api/v1/runs/${runId}`, hold);

  // 50 MiB of 100-byte lines, then a sentinel the final screen must show.
  await page.keyboard.type(`yes "$(printf 'x%.0s' $(seq 1 99))" | head -c 52428800; echo; echo flow-done-$((6*7))`);
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 120_000 }).toContain("flow-done-42");
  const worst = await page.evaluate(() =>
    (window as unknown as { __flowWorst: () => { what: string; ms: number } }).__flowWorst(),
  );
  releasePoll();
  await page.unroute(`**/api/v1/runs/${runId}`, hold);
  test.info().annotations.push({ type: "main-thread worst", description: `${worst.what}: ${Math.round(worst.ms)} ms` });
  expect(worst.ms, `longest main-thread work while streaming: ${worst.what}`).toBeLessThan(MAIN_THREAD_BUDGET_MS);

  // A backlog past the watermark must have been paused, and every pause ended resumed.
  const peak = await page.evaluate(() => (window as unknown as { __flowPeak: number }).__flowPeak);
  if (peak > FLOW_HIGH_WATERMARK) {
    expect(frames.pause, `backlog peaked at ${peak} bytes, past the ${FLOW_HIGH_WATERMARK} watermark`).toBeGreaterThan(0);
  }
  await expect.poll(() => frames.resume).toBe(frames.pause);

  // Input echo once the stream is over, recorded beside its own baseline, then held to ECHO_BUDGET_MS: from the Enter
  // keydown to the frame after xterm parsed the shell's answer (ECHO_BUDGET_MS over the baseline), timed in the page
  // so the harness's own round trips to it are not counted. The answer
  // (flow-echo-42) differs from what the typed line shows, so only it counts.
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });
  const echoMs = await timeEcho(page, "flow-echo");
  test.info().annotations.push({ type: "input-to-echo", description: `${Math.round(echoMs)} ms` });
  test.info().annotations.push({ type: "input-to-echo baseline", description: `${Math.round(baselineMs)} ms` });
  expect(echoMs, `input-to-echo ms (baseline ${Math.round(baselineMs)} ms)`).toBeLessThan(baselineMs + ECHO_BUDGET_MS);
});
