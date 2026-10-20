/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS byte parity for the run-mode refusals. Run:
//   cd ui && pnpm vitest run src/app/lib/run-mode-refusals.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./run-mode-refusals.golden.json";
import { RUN_MODE_REASON, RUN_MODE_REFUSAL, RUN_MODE_REFUSAL_BY_KEY, startFolderSubpathOk } from "./run-mode-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();

describe("run-mode refusal sentences", () => {
  it("are the golden bytes the server's twin is pinned to", () => {
    expect(golden.map((g) => g.key).sort()).toEqual(Object.keys(RUN_MODE_REFUSAL).sort());
    for (const g of golden) {
      expect(RUN_MODE_REFUSAL_BY_KEY[g.key as keyof typeof RUN_MODE_REFUSAL](...g.args), g.key).toBe(g.text);
    }
  });
});

describe("run-mode reasons", () => {
  it("are the Go constants of internal/api/reasons.go", () => {
    const go = readFileSync(join(root, "internal/api/reasons.go"), "utf8");
    const block = /The 0\.9 run-mode contract[\s\S]*?\n\t\/\/ validateWorkspaceSources/.exec(go)?.[0] ?? "";
    const goReasons = [...block.matchAll(/=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons.length).toBeGreaterThanOrEqual(5);
    expect(new Set(Object.values(RUN_MODE_REASON))).toEqual(new Set(goReasons));
  });
});

describe("start folder subpath", () => {
  it("is the server's rule: relative, cleaned, no .. element", () => {
    for (const ok of ["", "src", "src/app", "a.b/c-d", "..hidden"]) expect(startFolderSubpathOk(ok), ok).toBe(true);
    for (const bad of ["/etc", "../etc", "a/../b", "a//b", "a/", "./a", ".", "a\\b", "a\u0000b", "x".repeat(513)]) {
      expect(startFolderSubpathOk(bad), JSON.stringify(bad)).toBe(false);
    }
  });
});
