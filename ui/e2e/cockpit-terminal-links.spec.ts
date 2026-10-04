/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * LINKS IN THE BROWSER TERMINAL (term-t13), hermetic.
 *
 * Every link is text the sandbox printed, so each case plays the hostile pane:
 * an allowlisted device-login link opens in one click, noopener and noreferrer;
 * every other http(s) link, the console's own origin included, asks first; a
 * `javascript:` OSC 8 link and a userinfo-spoofed look-alike never open and
 * never match the allowlist. window.open is recorded, not performed.
 */
import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";

type Opened = [string, string, string];
const osc8 = (uri: string, text: string) => `\x1b]8;;${uri}\x1b\\${text}\x1b]8;;\x1b\\`;

// `ready` is plain text from the last line, awaited so the click lands on rendered output.
async function open(page: Page, lines: string[], ready: string) {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  await stubAttachSocket(page, (_n, ws: WebSocketRoute) => {
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from(lines.map((l) => `${l}\r\n`).join("")));
  });
  // clickLink measures the rendered text through the DOM rows, which only the
  // Compatible renderer draws: pin it before the first load.
  await page.addInitScript(() => localStorage.setItem("wardyn.terminal.renderer", "compatible"));
  await page.addInitScript(() => {
    const w = window as unknown as { __opened: Array<[string, string, string]> };
    w.__opened = [];
    window.open = ((url?: string | URL, target?: string, features?: string) => {
      w.__opened.push([String(url), String(target), String(features)]);
      return null;
    }) as typeof window.open;
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible();
  await expect.poll(async () => await screen.innerText()).toContain(ready);
  // The web font swaps in after first paint and refits the grid; xterm drops a hovered link on
  // every resize, so wait for the geometry to hold still before anyone hovers.
  let last = "";
  let steady = 0;
  while (steady < 6) {
    const now = `${await readGridText(page)} ${JSON.stringify(await screen.boundingBox())}`;
    steady = now === last ? steady + 1 : 0;
    last = now;
    await page.waitForTimeout(100);
  }
}
const readGridText = (page: Page) => page.getByTestId("run-terminal-pane").getByText(/^\d+×\d+$/).first().innerText();
const opened = (page: Page) => page.evaluate(() => (window as unknown as { __opened: Opened[] }).__opened);

/** Hover, then click, `chars` characters into the first terminal row whose text contains `needle`.
 *  The point comes from a DOM Range over the rendered text, so it stays right however the
 *  font settles the grid. */
async function clickLink(page: Page, needle: string, chars = 2) {
  const p = await page.evaluate(
    ([n, k]) => {
      for (const row of Array.from(document.querySelectorAll(".xterm-rows > div"))) {
        const w = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
        for (let t = w.nextNode(); t; t = w.nextNode()) {
          const i = (t.textContent ?? "").indexOf(n);
          if (i < 0) continue;
          const r = document.createRange();
          r.setStart(t, i + k);
          r.setEnd(t, i + k + 1);
          const b = r.getBoundingClientRect();
          return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
        }
      }
      return null;
    },
    [needle, chars] as const,
  );
  if (!p) throw new Error(`no terminal row contains ${needle}`);
  // Enter the link from the neighbouring cell: xterm re-evaluates only when the cell changes.
  await page.mouse.move(p.x - 9, p.y);
  await page.mouse.move(p.x, p.y);
  await page.mouse.click(p.x, p.y);
}

test("an allowlisted device-login link opens in one click with noopener,noreferrer and no dialog", async ({ page }) => {
  await open(page, ["go to https://github.com/login/device now"], "now");
  await clickLink(page, "https://github.com/login/device");
  await expect.poll(async () => await opened(page)).toEqual([["https://github.com/login/device", "_blank", "noopener,noreferrer"]]);
  await expect(page.getByTestId("terminal-link-dialog")).toHaveCount(0);
});

test("any other link, same-origin included, asks first; Cancel opens nothing and Open link opens noopener", async ({ page, baseURL }) => {
  await open(page, ["see https://example.com/docs?a=1 here", `console ${new URL(baseURL!).origin}/runs end`], "/runs end");
  await clickLink(page, "https://example.com/docs");
  const dialog = page.getByTestId("terminal-link-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId("terminal-link-url")).toHaveText("https://example.com/docs?a=1");
  await expect(dialog.getByTestId("terminal-link-host")).toHaveText("example.com");
  await expect(dialog.getByTestId("terminal-link-userinfo")).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeFocused();
  expect(await opened(page)).toEqual([]);
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toHaveCount(0);
  expect(await opened(page)).toEqual([]);

  await clickLink(page, "https://example.com/docs");
  await page.getByRole("button", { name: "Open link" }).click();
  expect(await opened(page)).toEqual([["https://example.com/docs?a=1", "_blank", "noopener,noreferrer"]]);

  // The console's own origin is never exempt.
  await clickLink(page, `${new URL(baseURL!).origin}/runs`);
  await expect(page.getByTestId("terminal-link-dialog")).toBeVisible();
  expect(await opened(page)).toHaveLength(1);
});

test("a userinfo look-alike of the allowlist asks, naming the real host, in both link kinds", async ({ page }) => {
  const spoof = "https://github.com@evil.example/login/device";
  await open(page, [`detected ${spoof} end`, `${osc8(spoof, "oscclick")} tail`], "oscclick tail");
  await clickLink(page, spoof, 20);
  const dialog = page.getByTestId("terminal-link-dialog");
  await expect(dialog.getByTestId("terminal-link-url")).toHaveText(spoof);
  await expect(dialog.getByTestId("terminal-link-host")).toHaveText("evil.example");
  await expect(dialog.getByTestId("terminal-link-userinfo")).toHaveText(
    "This address has a name before the host. The site it opens is evil.example.",
  );
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toHaveCount(0);

  await clickLink(page, "oscclick", 3);
  await expect(dialog.getByTestId("terminal-link-host")).toHaveText("evil.example");
  expect(await opened(page)).toEqual([]);
});

test("an OSC 8 javascript: link never opens and never asks, while an https one beside it does ask", async ({ page }) => {
  await open(
    page,
    [`${osc8("javascript:window.__pwned=1", "pwnclick")} ${osc8("https://example.com/ok", "okclick")} tail`],
    "pwnclick okclick tail",
  );
  await clickLink(page, "pwnclick", 3);
  await page.waitForTimeout(500);
  await expect(page.getByTestId("terminal-link-dialog")).toHaveCount(0);
  expect(await opened(page)).toEqual([]);
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  // The control: the same gesture on a neighbouring https link does reach the dialog.
  await clickLink(page, "okclick", 3);
  await expect(page.getByTestId("terminal-link-dialog").getByTestId("terminal-link-host")).toHaveText("example.com");
});
