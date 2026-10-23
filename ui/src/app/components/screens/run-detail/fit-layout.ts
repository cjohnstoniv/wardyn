/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3038: a finished run's widgets are as tall as their content, so nothing is clipped and an empty one does not
// spend a viewport-twelfth box on a sentence. The grid still counts rows; this turns a measured pixel height into
// rows and settles the tiles under each other once their heights are known.

export type FitItem = { i: string; x: number; y: number; w: number; h: number; minW?: number; minH?: number };

/** Rows a block of `px` pixels needs: a tile of h rows is h * rowHeight + (h - 1) * margin tall. */
export function fitRows(px: number, rowHeight: number, margin: number): number {
  return Math.max(1, Math.ceil((px + margin) / (rowHeight + margin)));
}

/**
 * The natural height of a widget tile: its card's header plus whatever its body holds, however tall the tile is
 * now. The body's children flow from the top, so the bottom of the last one is the content's height. 0 = unknown.
 */
export function measureTile(tile: HTMLElement): number {
  const card = tile.querySelector("section");
  const body = card?.lastElementChild as HTMLElement | null | undefined;
  const head = card?.firstElementChild as HTMLElement | null | undefined;
  if (!card || !body || !head || body === head) return 0;
  const last = body.lastElementChild as HTMLElement | null;
  if (!last) return 0;
  const top = body.getBoundingClientRect().top;
  const bottom = last.getBoundingClientRect().bottom;
  if (bottom <= top) return 0;
  const pad = parseFloat(getComputedStyle(body).paddingBottom) || 0;
  const border = card.offsetHeight - card.clientHeight;
  return Math.ceil(head.offsetHeight + (bottom - top) + pad + border);
}

/**
 * Give each measured tile its content's rows (the pinned ones keep theirs), then let every tile fall to the first
 * row free under whatever shares its columns. Tiles are settled in reading order, so the arrangement keeps its shape.
 */
export function settleItems(items: readonly FitItem[], rows: Readonly<Record<string, number>>, pinned: readonly string[]): FitItem[] {
  const sized = items.map((it) => {
    const fit = rows[it.i];
    return fit && !pinned.includes(it.i) ? { ...it, h: Math.max(fit, it.minH ?? 1) } : it;
  });
  const placed: FitItem[] = [];
  for (const it of [...sized].sort((a, b) => a.y - b.y || a.x - b.x)) {
    const y = placed
      .filter((p) => p.x < it.x + it.w && it.x < p.x + p.w)
      .reduce((bottom, p) => Math.max(bottom, p.y + p.h), 0);
    placed.push({ ...it, y });
  }
  return sized.map((it) => placed.find((p) => p.i === it.i)!);
}
