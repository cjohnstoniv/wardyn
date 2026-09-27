/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";

// F7-F16 — before this, index.html had no <link rel="icon"> and public/ held
// only the two licence files: every tab showed the browser's generic
// blank-page icon. vitest's cwd is the `ui/` package root (vitest.config.ts).
describe("favicon", () => {
  // ticket: F7-F16
  it("public/favicon.svg exists", () => {
    expect(existsSync("public/favicon.svg")).toBe(true);
  });

  it("index.html links it", () => {
    const html = readFileSync("index.html", "utf8");
    expect(html).toMatch(/<link\s+rel="icon"[^>]*href="\/favicon\.svg"/);
  });
});
