/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS byte parity for the template refusals. Run:
//   cd ui && pnpm vitest run src/app/lib/template-refusals.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./template-refusals.golden.json";
import { TEMPLATE_REASON, TEMPLATE_REFUSAL, TEMPLATE_REFUSAL_BY_KEY, TEMPLATE_REFUSAL_REASON } from "./template-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

describe("template refusal sentences", () => {
  it("are the golden bytes the server's twin is pinned to", () => {
    expect(golden.map((g) => g.key).sort()).toEqual(Object.keys(TEMPLATE_REFUSAL).sort());
    for (const g of golden) {
      expect(TEMPLATE_REFUSAL_BY_KEY[g.key as keyof typeof TEMPLATE_REFUSAL](...g.args), g.key).toBe(g.text);
    }
  });

  it("each belongs to a reason the server sends", () => {
    for (const reason of Object.values(TEMPLATE_REFUSAL_REASON)) {
      expect(Object.values(TEMPLATE_REASON)).toContain(reason);
    }
  });

  it("never name a group or a person in a not-found or forbidden answer", () => {
    expect(TEMPLATE_REFUSAL.NOT_FOUND()).not.toMatch(/group|person/i);
    expect(TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_GROUP()).not.toMatch(/"|'/);
  });
});

describe("template reasons", () => {
  it("are the Go constants of internal/api/reasons.go", () => {
    const go = readFileSync(join(repoRoot(), "internal/api/reasons.go"), "utf8");
    const goReasons = [...go.matchAll(/\breasonTemplate\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons).toHaveLength(17);
    expect(new Set(Object.values(TEMPLATE_REASON))).toEqual(new Set(goReasons));
  });
});
