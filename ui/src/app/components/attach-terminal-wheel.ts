/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Trackpad wheel -> terminal mouse reports. With the app owning the mouse (tmux),
// xterm turns every wheel event into its own report; a trackpad fires dozens per
// frame and floods the socket. This coalesces them: wheel delta accumulates and
// one frame per animation frame goes out, carrying one SGR wheel report per
// accumulated line, so no scroll distance is dropped.

import type { Terminal } from "@xterm/xterm";

// Pixels a trackpad must travel for one line; deltaMode 1 (lines) is exact, 2 (pages) is a screenful.
const PIXELS_PER_LINE = 40;

export function attachWheelCoalescer(
  term: Terminal,
  send: (payload: Uint8Array) => void,
  canScroll: () => boolean,
): () => void {
  let acc = 0;
  let at = { col: 1, row: 1 };
  let raf = 0;
  const flush = () => {
    raf = 0;
    const lines = Math.trunc(acc);
    if (lines === 0) return;
    acc -= lines;
    const report = `\x1b[<${lines < 0 ? 64 : 65};${at.col};${at.row}M`;
    send(new TextEncoder().encode(report.repeat(Math.abs(lines))));
  };
  term.attachCustomWheelEventHandler((e: WheelEvent) => {
    if (term.modes.mouseTrackingMode === "none") return true; // xterm's own viewport scroll
    if (!canScroll()) return false; // an observer never scrolls
    const unit = e.deltaMode === 1 ? 1 : e.deltaMode === 2 ? term.rows : 1 / PIXELS_PER_LINE;
    acc += e.deltaY * unit;
    const box = term.element?.querySelector(".xterm-screen")?.getBoundingClientRect();
    if (box && box.width > 0 && box.height > 0) {
      const col = Math.floor(((e.clientX - box.left) / box.width) * term.cols) + 1;
      const row = Math.floor(((e.clientY - box.top) / box.height) * term.rows) + 1;
      at = { col: Math.min(Math.max(col, 1), term.cols), row: Math.min(Math.max(row, 1), term.rows) };
    }
    if (!raf) raf = requestAnimationFrame(flush);
    return false;
  });
  return () => {
    if (raf) cancelAnimationFrame(raf);
  };
}
