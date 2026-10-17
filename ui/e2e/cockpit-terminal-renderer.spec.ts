/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * The terminal renderer (term-t10), hermetic. A lost WebGL context falls back to
 * the DOM renderer with the session intact: the same socket, the same scrollback,
 * new output still drawn. The renderer menu stores its choice in this browser.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { termText } from "./terminal-text";
import { TERMINAL_RENDERER } from "../src/app/components/wardyn/copy";

const RENDERER_KEY = "wardyn.terminal.renderer";

test("a lost GPU context falls back to the DOM renderer and the session carries on", async ({ page }) => {
  await page.addInitScript(([k, v]) => localStorage.setItem(k, v), [RENDERER_KEY, "gpu"]);
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  let live: { send: (b: Buffer) => void } | undefined;
  const sock = await stubAttachSocket(page, (_n, ws) => {
    live = { send: (b) => ws.send(b) };
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from("\x1b[2J\x1b[Hbefore the loss\r\n", "utf8"));
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = page.getByTestId("run-terminal-pane").locator(".xterm-screen").first();
  await expect(screen.locator("canvas:not([class*='-layer'])")).toHaveCount(1);
  await expect.poll(() => termText(screen)).toContain("before the loss");

  await page.evaluate(() => {
    const gl = document.querySelector<HTMLCanvasElement>("canvas:not([class*='-layer'])")!.getContext("webgl2")!;
    gl.getExtension("WEBGL_lose_context")!.loseContext();
  });

  await expect(page.getByTestId("terminal-renderer-notice")).toHaveText(new RegExp(TERMINAL_RENDERER.FELL_BACK));
  await expect(screen.locator("canvas:not([class*='-layer'])")).toHaveCount(0);
  // The session is intact: earlier output is still there, new output draws, one socket was ever opened.
  live!.send(Buffer.from("after the loss\r\n", "utf8"));
  await expect.poll(() => termText(screen)).toContain("after the loss");
  expect(await termText(screen)).toContain("before the loss");
  expect(sock.opens()).toBe(1);
});

test("the renderer menu stores the choice in this browser and applies it at once", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(false)));
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);

  const screen = page.getByTestId("run-terminal-pane").locator(".xterm-screen").first();
  await expect(screen.locator("canvas:not([class*='-layer'])")).toHaveCount(1); // Auto takes the GPU where WebGL2 works

  await page.getByRole("button", { name: TERMINAL_RENDERER.LABEL }).click();
  await page.getByRole("menuitemradio", { name: new RegExp(`^${TERMINAL_RENDERER.COMPATIBLE}`) }).click();
  await expect(screen.locator("canvas:not([class*='-layer'])")).toHaveCount(0);
  expect(await page.evaluate((k) => localStorage.getItem(k), RENDERER_KEY)).toBe("compatible");

  await page.getByRole("button", { name: TERMINAL_RENDERER.LABEL }).click();
  await expect(page.getByTestId("terminal-renderer-footer")).toHaveText(TERMINAL_RENDERER.FOOTER("Compatible"));
});
