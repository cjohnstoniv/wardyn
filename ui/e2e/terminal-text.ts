/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Terminal text for specs. The GPU renderer draws to a canvas, so the DOM has no
// text to read; the e2e UI build (`--mode e2e`) registers each terminal on
// window.__wardynTerm and this reads its buffer. Against a production bundle
// (no registry) it falls back to the DOM, which only the compatible renderer fills.
import type { Locator } from "@playwright/test";

type Term = {
  element?: HTMLElement;
  rows: number;
  buffer: { active: { viewportY: number; getLine(y: number): { translateToString(trim: boolean): string } | undefined } };
};

/**
 * The visible rows of the terminal that contains `screen` (its `.xterm-screen`,
 * or any element inside the terminal). The bounded default timeout keeps a
 * detached node from stalling a poll.
 */
export async function termRows(screen: Locator, timeout = 1_000): Promise<string[]> {
  return screen.evaluate(
    (el, regName) => {
      const reg = (window as unknown as Record<string, { terms: Set<Term> } | undefined>)[regName];
      const host = el.closest(".xterm");
      if (reg) {
        for (const t of reg.terms) {
          if (t.element && t.element === host) {
            const b = t.buffer.active;
            const out: string[] = [];
            for (let i = 0; i < t.rows; i++) out.push(b.getLine(b.viewportY + i)?.translateToString(true) ?? "");
            return out;
          }
        }
      }
      return [...(host ?? el).querySelectorAll(".xterm-rows > div")].map((r) => r.textContent ?? "");
    },
    "__wardynTerm",
    { timeout },
  );
}

/** The visible terminal text, one row per line (what `innerText` gave under the DOM renderer). */
export async function termText(screen: Locator, timeout = 1_000): Promise<string> {
  return (await termRows(screen, timeout)).join("\n");
}
