/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Box-drawing and block glyphs (U+2500-259F) are exact in both renderers (term-t10).
 * The stub prints all 160 code points as ten rows of sixteen; the grid is cropped
 * and compared, per renderer, with its own committed golden (terminal-glyphs-gpu.png,
 * terminal-glyphs-compatible.png) within GLYPH_DIFF_RATIO, because the GPU and DOM
 * cell heights differ. The OS fallback font (what the terminal used before the
 * font was self-hosted) draws these glyphs differently and exceeds it.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { readGrid } from "./terminal-grid";
import { termText } from "./terminal-text";

// Fraction of pixels allowed to differ (antialiasing, subpixel placement). Stated, not tuned per run.
const GLYPH_DIFF_RATIO = 0.04;
const RENDERER_KEY = "wardyn.terminal.renderer";

function glyphRows(): string {
  let out = "\x1b[2J\x1b[H\x1b[?25l";
  for (let row = 0; row < 10; row++) {
    let line = "";
    for (let col = 0; col < 16; col++) line += String.fromCodePoint(0x2500 + row * 16 + col);
    out += line + "\r\n";
  }
  return out + "END";
}

for (const renderer of ["compatible", "gpu"] as const) {
  test(`box-drawing and block glyphs match the golden under the ${renderer} renderer`, async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.addInitScript(([k, v]) => localStorage.setItem(k, v), [RENDERER_KEY, renderer]);
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);
    await stubAttachSocket(page, (_n, ws) => {
      ws.send(attachModeFrame(false));
      ws.send(Buffer.from(glyphRows(), "utf8"));
    });
    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);

    const screen = page.getByTestId("run-terminal-pane").locator(".xterm-screen").first();
    await expect(screen).toBeVisible();
    await expect.poll(() => termText(screen)).toContain("END");
    // The renderer under test is the one drawing.
    await expect(screen.locator("canvas:not([class*='-layer'])")).toHaveCount(renderer === "gpu" ? 1 : 0);
    await page.evaluate(() => document.fonts.load("13px 'JetBrains Mono Terminal'"));

    // The cell size settles once the font is in and the grid has refit: wait for two equal samples.
    let cw = 0;
    let ch = 0;
    let x = 0;
    let y = 0;
    await expect
      .poll(async () => {
        const grid = await readGrid(page);
        const box = (await screen.boundingBox())!;
        const next = [box.width / grid.cols, box.height / grid.rows].map((v) => Math.round(v * 100) / 100);
        const same = next[0] === cw && next[1] === ch && box.x === x && box.y === y;
        [cw, ch, x, y] = [next[0], next[1], box.x, box.y];
        return same;
      }, { intervals: [300], timeout: 10_000 })
      .toBe(true);
    const shot = await page.screenshot({
      clip: { x, y, width: Math.floor(cw * 16), height: Math.floor(ch * 10) },
      animations: "disabled",
    });
    expect(shot).toMatchSnapshot(`terminal-glyphs-${renderer}.png`, { maxDiffPixelRatio: GLYPH_DIFF_RATIO });
  });
}
