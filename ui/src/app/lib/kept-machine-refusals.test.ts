/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { KEPT_MACHINE_REASON } from "./kept-machine-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

describe("kept-machine reasons", () => {
  it("are the Go constants of internal/api/reasons_kept_machine.go", () => {
    const go = readFileSync(join(repoRoot(), "internal/api/reasons_kept_machine.go"), "utf8");
    const reasons = [...go.matchAll(/=\s*"(kept_machine_[a-z_]+)"/g)].map((m) => m[1]);
    expect(reasons.length).toBeGreaterThanOrEqual(20);
    expect(new Set(Object.values(KEPT_MACHINE_REASON))).toEqual(new Set(reasons));
  });
});
