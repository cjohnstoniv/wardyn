/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// F7-F10/F7-F11 — source-scan pins on the two class-string primitives, the
// same discipline theme-contrast.test.ts uses for theme.css: cheap, exact,
// and immune to Radix's own jsdom rendering gaps.
const tabsSrc = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "tabs.tsx"), "utf8");

function classesFor(component: string): string[] {
  const re = new RegExp(`function ${component}\\([\\s\\S]*?cn\\(\\s*"([^"]+)"`);
  const m = re.exec(tabsSrc);
  if (!m) throw new Error(`${component} className not found`);
  return m[1].split(/\s+/);
}

describe("TabsList — F7-F11: the stray flex line-drift", () => {
  it("carries inline-flex, not a redundant bare `flex` token", () => {
    const classes = classesFor("TabsList");
    expect(classes).toContain("inline-flex");
    expect(classes).not.toContain("flex");
  });
});

describe("TabsContent — F7-F10: outline-none with no replacement focus ring", () => {
  it("carries a focus-visible ring — outline-none alone drops the ONLY visible focus indicator", () => {
    const classes = classesFor("TabsContent");
    expect(classes).toContain("outline-none");
    expect(classes.some((c) => c.startsWith("focus-visible:ring"))).toBe(true);
  });
});
