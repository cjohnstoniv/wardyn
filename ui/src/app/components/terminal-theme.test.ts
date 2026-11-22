/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { TERMINAL_THEME } from "./terminal-theme";

// #3078: xterm cannot read CSS variables, so its palette is literal hex in terminal-theme.ts. This holds the two
// colours theme.css shares with it equal, keeps the terminal near-black in both themes, and keeps the terminal
// colours from being typed again anywhere else.

const css = readFileSync("src/styles/theme.css", "utf8");
const rootStart = css.indexOf(":root");
const root = css.slice(rootStart, css.indexOf("\n}", rootStart));
const darkStart = css.indexOf("\n.dark {");
const dark = css.slice(darkStart, css.indexOf("\n}", darkStart));

function token(block: string, name: string): string | undefined {
  return block.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})\\s*;`))?.[1];
}

// Follow-up: these three run-detail sites still type the terminal colours; the next commit moves them to the tokens.
const FOLLOW_UP = ["run-detail/output-tab.tsx", "run-detail/output-pane.tsx", "run-detail/terminal-notice.tsx"];

describe("terminal palette", () => {
  it("keeps theme.css's --terminal and --terminal-foreground equal to the xterm palette", () => {
    expect(token(root, "terminal")).toBe(TERMINAL_THEME.background);
    expect(token(root, "terminal-foreground")).toBe(TERMINAL_THEME.foreground);
  });

  it("sets no terminal token in .dark (the terminal is near-black in both themes)", () => {
    expect(darkStart).toBeGreaterThan(-1);
    expect(dark).not.toMatch(/--terminal/);
  });

  it("keeps the terminal colours in terminal-theme.ts and theme.css, nowhere else", () => {
    const banned = ["#0d1117", "#e6edf3", "#ff5f56", "#ffbd2e", "#28c840"];
    const found: string[] = [];
    const walk = (dir: string) => {
      for (const e of readdirSync(dir, { withFileTypes: true })) {
        const p = join(dir, e.name);
        if (e.isDirectory()) walk(p);
        else if (/\.tsx?$/.test(e.name) && !/\.test\./.test(e.name) && !p.endsWith("components/terminal-theme.ts") && !FOLLOW_UP.some((f) => p.endsWith(f))) {
          const text = readFileSync(p, "utf8").toLowerCase();
          for (const b of banned) if (text.includes(b)) found.push(`${p} ${b}`);
        }
      }
    };
    walk("src/app");
    expect(found).toEqual([]);
  });
});
