/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS byte parity for the drives[] refusals. Run:
//   cd ui && pnpm vitest run src/app/lib/drive-refusals.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./drive-refusals.golden.json";
import { DRIVE_REFUSAL, DRIVE_REFUSAL_REASON } from "./drive-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

describe("drive refusal sentences", () => {
  it("are the golden bytes the server's twin is pinned to", () => {
    expect(golden.map((g) => g.key).sort()).toEqual(Object.keys(DRIVE_REFUSAL).sort());
    for (const g of golden) {
      const sentence = DRIVE_REFUSAL[g.key as keyof typeof DRIVE_REFUSAL] as (...args: string[]) => string;
      expect(sentence(...g.args), g.key).toBe(g.text);
    }
  });

  it("each reason is a Go constant of internal/api/reasons.go or the authz registry", () => {
    const goReasons =
      readFileSync(join(repoRoot(), "internal/api/reasons.go"), "utf8") +
      readFileSync(join(repoRoot(), "internal/authz/registry.go"), "utf8");
    for (const reason of new Set(Object.values(DRIVE_REFUSAL_REASON))) {
      expect(goReasons, reason).toMatch(new RegExp(`= "${reason}"`));
    }
  });
});
