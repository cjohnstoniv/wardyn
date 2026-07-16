/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";

// C004: the light-theme (:root) semantic tokens are used as button/badge text
// (white on them) and as text/dot colors on white + their own -subtle tint, so each
// must clear WCAG AA 4.5:1 for normal text. This test reads theme.css and re-proves
// it, so a revert to the teal-500-family brights (success 2.25:1, warning 1.94:1)
// fails here. The dark theme already passes (6-8:1) and is out of scope.

function lum(hex: string): number {
  const ch = (i: number) => parseInt(hex.slice(i, i + 2), 16) / 255;
  const lin = (c: number) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
  return 0.2126 * lin(ch(1)) + 0.7152 * lin(ch(3)) + 0.0722 * lin(ch(5));
}
function ratio(a: string, b: string): number {
  const la = lum(a);
  const lb = lum(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

const css = readFileSync("src/styles/theme.css", "utf8");
// The light theme is the :root { ... } block; slice to its closing brace (the first
// "\n}" line) rather than to ".dark" — the header comments mention .dark, which would
// truncate the slice to nothing.
const rootStart = css.indexOf(":root");
const root = css.slice(rootStart, css.indexOf("\n}", rootStart));

function token(name: string): string {
  const m = root.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`));
  if (!m) throw new Error(`--${name} not found in :root`);
  return m[1];
}
// The -subtle tint composited over white — the actual background the text sits on.
function subtleBg(name: string): string {
  const m = root.match(new RegExp(`--${name}-subtle:\\s*rgba\\((\\d+),\\s*(\\d+),\\s*(\\d+),\\s*([\\d.]+)\\)`));
  if (!m) throw new Error(`--${name}-subtle not found in :root`);
  const [r, g, b, a] = [+m[1], +m[2], +m[3], +m[4]];
  const over = (v: number) => Math.round(a * v + (1 - a) * 255);
  const hex = (v: number) => v.toString(16).padStart(2, "0");
  return "#" + hex(over(r)) + hex(over(g)) + hex(over(b));
}

const WHITE = "#ffffff";

describe("light-theme WCAG AA contrast (C004)", () => {
  it("white button text on --primary is >= 4.5:1", () => {
    expect(ratio(WHITE, token("primary"))).toBeGreaterThanOrEqual(4.5);
  });

  it("white text on --danger (KILLED badge) is >= 4.5:1", () => {
    expect(ratio(WHITE, token("danger"))).toBeGreaterThanOrEqual(4.5);
  });

  for (const t of ["success", "warning", "danger"]) {
    it(`--${t} text is >= 4.5:1 on white and on its own -subtle tint`, () => {
      expect(ratio(token(t), WHITE)).toBeGreaterThanOrEqual(4.5);
      expect(ratio(token(t), subtleBg(t))).toBeGreaterThanOrEqual(4.5);
    });
  }
});
