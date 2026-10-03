/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pointer helpers for the terminal copy specs: the user's drag, double-click
// and triple-click over xterm's grid, addressed by cell.
import type { Locator, Page } from "@playwright/test";

export interface Grid {
  cols: number;
  rows: number;
}

/** The grid xterm reports in the header ("120×40"). */
export async function readGrid(page: Page): Promise<Grid> {
  const text = await page.getByTestId("run-terminal-pane").getByText(/^\d+×\d+$/).first().innerText();
  const [cols, rows] = text.split("×").map(Number);
  return { cols, rows };
}

/** The viewport point at the centre of cell (col, row) of the screen. */
export async function cellPoint(screen: Locator, grid: Grid, col: number, row: number) {
  const box = (await screen.boundingBox())!;
  return {
    x: box.x + ((col + 0.5) * box.width) / grid.cols,
    y: box.y + ((row + 0.5) * box.height) / grid.rows,
  };
}

/** The row index, on the visible screen, of the first row whose text matches. */
export async function rowOf(screen: Locator, re: RegExp): Promise<number> {
  const i = await screen
    .locator(".xterm-rows > div")
    .evaluateAll((els, src) => els.findIndex((e) => new RegExp(src).test(e.textContent ?? "")), re.source);
  if (i < 0) throw new Error(`no screen row matches ${re}`);
  return i;
}

export async function dragCells(page: Page, screen: Locator, grid: Grid, from: [number, number], to: [number, number]) {
  const a = await cellPoint(screen, grid, from[0], from[1]);
  const b = await cellPoint(screen, grid, to[0], to[1]);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move((a.x + b.x) / 2, (a.y + b.y) / 2, { steps: 3 });
  await page.mouse.move(b.x, b.y, { steps: 3 });
  await page.mouse.up();
}

export async function clickCell(page: Page, screen: Locator, grid: Grid, at: [number, number], clickCount: 2 | 3) {
  const p = await cellPoint(screen, grid, at[0], at[1]);
  await page.mouse.click(p.x, p.y, { clickCount });
}
