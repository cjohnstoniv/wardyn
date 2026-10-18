/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

function walkTsx(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    return e.isDirectory() ? walkTsx(p) : p.endsWith(".tsx") && !p.endsWith(".test.tsx") ? [p] : [];
  });
}

describe("date-cell hovers", () => {
  // The relative date in a cell hovers as absoluteTime(), never the wire ISO string.
  it("no title={x.created_at|updated_at|proposed_at} shows a raw ISO timestamp", () => {
    const raw = /\btitle=\{[\w.?]+\.(?:created_at|updated_at|proposed_at)\}/;
    const offenders: string[] = [];
    for (const f of walkTsx("src/app")) {
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          if (raw.test(ln)) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(offenders, `wrap in absoluteTime():\n${offenders.join("\n")}`).toHaveLength(0);
  });
});
