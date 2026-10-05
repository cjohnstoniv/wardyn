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

  // While it streams, none of the work the stream drives holds the main thread
  // past the budget: the socket handler, xterm's parse slices (timers) and its
  // drawing (animation frames), and style and layout. Long-animation-frame
  // entries report every one, so nothing is sampled. A timer round trip measured
  // the runner instead: the throttle spins the renderer, and a starved GPU
  // process stalls each commit. React renders here follow the page's polls, not
  // the output, so they are not counted.
  await page.evaluate(() => {
    type Frame = PerformanceEntry & {
      styleAndLayoutStart: number;
      scripts: Array<{ sourceURL: string; invoker: string; duration: number }>;
    };
    let worst = { what: "nothing over 50 ms", ms: 0 };
    const note = (what: string, ms: number) => {
      if (ms > worst.ms) worst = { what, ms };
    };
    // Every way terminal output reaches the page: use-attach-session.ts's socket handler, xterm's parse
    // timers and its draw frames. If the output path ever moves to another invoker (a Worker, a
    // MessagePort, idle callbacks), add it here, or this check passes on no data.
    const stream = ["DOMWebSocket.onmessage", "TimerHandler:setTimeout", "FrameRequestCallback"];
    const scan = (frames: PerformanceEntryList) => {
      for (const f of frames as Frame[]) {
        // The page's scripts: the harness's own evaluations have no source.
        for (const s of f.scripts) if (s.sourceURL && stream.includes(s.invoker)) note(s.invoker, s.duration);
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

  // 50 MiB of 100-byte lines, then a sentinel the final screen must show.
  await page.keyboard.type(`yes "$(printf 'x%.0s' $(seq 1 99))" | head -c 52428800; echo; echo flow-done-$((6*7))`);
  await page.keyboard.press("Enter");
  await expect.poll(read, { timeout: 120_000 }).toContain("flow-done-42");
  const worst = await page.evaluate(() =>
    (window as unknown as { __flowWorst: () => { what: string; ms: number } }).__flowWorst(),
  );
  test.info().annotations.push({ type: "main-thread worst", description: `${worst.what}: ${Math.round(worst.ms)} ms` });
  expect(worst.ms, `longest main-thread work while streaming: ${worst.what}`).toBeLessThan(MAIN_THREAD_BUDGET_MS);

  // A backlog past the watermark must have been paused, and every pause ended resumed.
  const peak = await page.evaluate(() => (window as unknown as { __flowPeak: number }).__flowPeak);
  if (peak > FLOW_HIGH_WATERMARK) {
    expect(frames.pause, `backlog peaked at ${peak} bytes, past the ${FLOW_HIGH_WATERMARK} watermark`).toBeGreaterThan(0);
  }
  await expect.poll(() => frames.resume).toBe(frames.pause);

  // And input still echoes promptly once the stream is over: from the Enter
  // keydown to the frame after xterm parsed the shell's answer, timed in the page
  // so the harness's own round trips to it are not counted. The answer
  // (flow-echo-42) differs from what the typed line shows, so only it counts.
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });
  await page.keyboard.type("echo flow-echo-$((6*7))");
  await page.evaluate(() => {
    type T = {
      rows: number;
      buffer: { active: { viewportY: number; getLine(y: number): { translateToString(trim: boolean): string } | undefined } };
      onWriteParsed(cb: () => void): { dispose(): void };
    };
    const w = window as unknown as { __wardynTerm: { terms: Set<T> }; __echoMs?: number };
    const term = [...w.__wardynTerm.terms][0]!;
    let t0 = 0;
    document.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !t0) t0 = performance.now();
    }, { capture: true });
    const parsed = term.onWriteParsed(() => {
      const b = term.buffer.active;
      for (let y = b.viewportY; t0 && y < b.viewportY + term.rows; y++) {
        if (b.getLine(y)?.translateToString(true).includes("flow-echo-42")) {
          parsed.dispose();
          requestAnimationFrame(() => (w.__echoMs = performance.now() - t0));
          return;
        }
      }
    });
  });
  await page.keyboard.press("Enter");
  const echo = await page.waitForFunction(() => (window as unknown as { __echoMs?: number }).__echoMs, null, { timeout: 5_000 });
  const echoMs = (await echo.jsonValue()) as number;
  test.info().annotations.push({ type: "input-to-echo", description: `${Math.round(echoMs)} ms` });
  expect(echoMs, "input-to-echo ms").toBeLessThan(ECHO_BUDGET_MS);
});
